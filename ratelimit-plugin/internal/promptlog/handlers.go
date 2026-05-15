package promptlog

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"golang.org/x/crypto/bcrypt"

	"github.com/cyberk/ratelimit-plugin/internal/ratelimit"
)

// RegisterReadHandlers exposes the read-only prompt log under
// /v0/management/prompts/*. Auth mirrors usagestore's management-key
// middleware so operators only configure one secret.
func RegisterReadHandlers(engine *gin.Engine, proxyCfg *config.Config, plogCfg *Config) {
	if plogCfg == nil || !plogCfg.IsEnabled() {
		// Endpoints are registered anyway so the UI gets a clean 503 instead
		// of 404 — easier to detect "feature off" vs "wrong URL".
		engine.GET("/v0/management/prompts/users", disabledHandler)
		engine.GET("/v0/management/prompts/users/:key", disabledHandler)
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

		limit := 200
		if v := c.Query("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
				limit = n
			}
		}

		detail, err := BuildDetail(plogCfg.Dir, keyHash, hint, configured, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
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
