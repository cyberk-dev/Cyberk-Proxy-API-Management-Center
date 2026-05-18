package promptlog

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"golang.org/x/crypto/bcrypt"

	"github.com/cyberk/ratelimit-plugin/internal/ratelimit"
)

// RegisterReadHandlers exposes the read-only prompt log under
// /v0/management/prompts/*. Auth mirrors usagestore's management-key
// middleware so operators only configure one secret. templates may be nil
// (templating disabled) — in that case the templates endpoints return 503.
func RegisterReadHandlers(engine *gin.Engine, proxyCfg *config.Config, plogCfg *Config, templates *TemplateStore) {
	if plogCfg == nil || !plogCfg.IsEnabled() {
		// Endpoints are registered anyway so the UI gets a clean 503 instead
		// of 404 — easier to detect "feature off" vs "wrong URL".
		engine.GET("/v0/management/prompts/users", disabledHandler)
		engine.GET("/v0/management/prompts/users/:key", disabledHandler)
		engine.GET("/v0/management/prompts/templates", disabledHandler)
		engine.GET("/v0/management/prompts/templates/:hash", disabledHandler)
		return
	}

	auth := makeAuthMiddleware(proxyCfg)

	engine.GET("/v0/management/prompts/users", auth, func(c *gin.Context) {
		users, err := ListUsers(plogCfg.Dir, configuredKeys(proxyCfg))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"users": users})
	})

	engine.GET("/v0/management/prompts/templates", auth, func(c *gin.Context) {
		if templates == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "templates feature is disabled"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"templates": templates.List()})
	})

	engine.GET("/v0/management/prompts/templates/:hash", auth, func(c *gin.Context) {
		if templates == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "templates feature is disabled"})
			return
		}
		hash := strings.ToLower(strings.TrimSpace(c.Param("hash")))
		if hash == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "missing hash"})
			return
		}
		t, ok := templates.Get(hash)
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "template not found"})
			return
		}
		c.JSON(http.StatusOK, t)
	})

	engine.GET("/v0/management/prompts/users/:key", auth, func(c *gin.Context) {
		raw := c.Param("key")
		if raw == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "missing key"})
			return
		}

		var keyHash string
		var hint string
		var configured bool
		if IsHexKeyHash(raw) {
			keyHash = strings.ToLower(raw)
			// Cross-reference with configured keys for hint.
			for _, k := range configuredKeys(proxyCfg) {
				if ratelimit.HashKey(k) == keyHash {
					hint = MakeKeyHint(k)
					configured = true
					break
				}
			}
		} else {
			keyHash = ratelimit.HashKey(raw)
			hint = MakeKeyHint(raw)
			for _, k := range configuredKeys(proxyCfg) {
				if k == raw {
					configured = true
					break
				}
			}
		}

		opts := DetailOpts{
			MessageLimit: 200,
			SessionLimit: 200,
			InitialCWDs:  20,
		}
		if v := c.Query("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
				opts.MessageLimit = n
			}
		}
		if v := c.Query("session_limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
				opts.SessionLimit = n
			}
		}
		if v := c.Query("initial_cwds"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 100 {
				opts.InitialCWDs = n
			}
		}
		opts.CWDFilter = strings.TrimSpace(c.Query("cwd"))
		opts.HeadersOnly = c.Query("headers_only") == "1" || c.Query("headers_only") == "true"
		opts.SessionFilter = strings.TrimSpace(c.Query("session_id"))
		if opts.SessionFilter != "" && opts.CWDFilter == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "session_id requires cwd"})
			return
		}
		if mbRaw := strings.TrimSpace(c.Query("message_before")); mbRaw != "" {
			if opts.SessionFilter == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "message_before requires session_id"})
				return
			}
			if opts.HeadersOnly {
				c.JSON(http.StatusBadRequest, gin.H{"error": "message_before is meaningless with headers_only"})
				return
			}
			mb, err := time.Parse(time.RFC3339Nano, mbRaw)
			if err != nil {
				if mb, err = time.Parse(time.RFC3339, mbRaw); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": "message_before not RFC3339"})
					return
				}
			}
			opts.MessageBefore = mb
		}

		// session_before is composite: "<RFC3339>|<session_id>". Strict-
		// less-than on timestamp alone would drop sessions tied at the
		// same last_seen — the cursor tie-breaks on session_id.
		//
		// SplitN with n=2 splits at the FIRST '|' only: the unsplit
		// remainder is parts[1]. So a session_id that itself contains '|'
		// (rare but allowed — session IDs are arbitrary strings on the
		// wire) is preserved end-to-end. Don't change to Split() or you
		// will silently break those cursors.
		if raw := c.Query("session_before"); raw != "" {
			if opts.CWDFilter == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "session_before requires cwd"})
				return
			}
			if opts.HeadersOnly {
				c.JSON(http.StatusBadRequest, gin.H{"error": "session_before is meaningless with headers_only"})
				return
			}
			// session_before paginates SESSIONS within a CWD; session_id
			// targets ONE session for message-paging. Combining them is
			// self-contradictory — either the cursor filters out the
			// targeted session (empty Sessions, SessionCount > 0) or it
			// doesn't, and the client can't tell which silently. Reject
			// so the caller fixes the contradiction at source.
			if opts.SessionFilter != "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "session_before is mutually exclusive with session_id"})
				return
			}
			parts := strings.SplitN(raw, "|", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "session_before must be '<RFC3339>|<session_id>'"})
				return
			}
			ts, err := time.Parse(time.RFC3339Nano, parts[0])
			if err != nil {
				if ts, err = time.Parse(time.RFC3339, parts[0]); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": "session_before timestamp not RFC3339"})
					return
				}
			}
			opts.SessionBefore = &SessionCursor{Ts: ts, Sid: parts[1]}
		}

		detail, err := BuildDetail(plogCfg.Dir, keyHash, hint, configured, opts)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if c.Query("inline_templates") == "1" {
			InlineTemplates(detail, templates)
		}
		c.JSON(http.StatusOK, detail)
	})
}

func disabledHandler(c *gin.Context) {
	c.JSON(http.StatusServiceUnavailable, gin.H{
		"error":   "prompt_log feature is disabled",
		"enabled": false,
	})
}

func configuredKeys(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	return cfg.APIKeys
}

// makeAuthMiddleware is a duplicated copy of usagestore.makeAuthMiddleware —
// kept in-package to avoid coupling promptlog read handlers to the unrelated
// usagestore lifecycle. The two should track each other on auth changes.
func makeAuthMiddleware(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		secretKey := ""
		if cfg != nil {
			secretKey = cfg.RemoteManagement.SecretKey
		}
		if secretKey == "" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "remote management key not configured"})
			return
		}

		provided := c.GetHeader("X-Management-Key")
		if provided == "" {
			if ah := c.GetHeader("Authorization"); ah != "" {
				parts := strings.SplitN(ah, " ", 2)
				if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
					provided = parts[1]
				}
			}
		}
		if provided == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing management key"})
			return
		}

		if subtle.ConstantTimeCompare([]byte(provided), []byte(secretKey)) == 1 {
			c.Next()
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(secretKey), []byte(provided)) == nil {
			c.Next()
			return
		}

		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid management key"})
	}
}
