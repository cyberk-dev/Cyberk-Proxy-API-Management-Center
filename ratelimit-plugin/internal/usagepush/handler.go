package usagepush

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"golang.org/x/crypto/bcrypt"
)

type incomingRecord struct {
	Timestamp   string          `json:"timestamp"`
	LatencyMs   int64           `json:"latency_ms"`
	Source      string          `json:"source"`
	AuthIndex   string          `json:"auth_index"`
	Tokens      json.RawMessage `json:"tokens"`
	Failed      bool            `json:"failed"`
	Provider    string          `json:"provider"`
	Model       string          `json:"model"`
	Alias       string          `json:"alias"`
	Endpoint    string          `json:"endpoint"`
	AuthType    string          `json:"auth_type"`
	APIKey      string          `json:"api_key"`
	RequestID   string          `json:"request_id"`
}

type tokenFields struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
}

func Register(engine *gin.Engine, cfg *config.Config) {
	engine.POST("/v0/management/usage-queue", makeHandler(cfg))
}

func makeHandler(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !authenticate(c, cfg) {
			return
		}

		var records []incomingRecord
		if err := c.ShouldBindJSON(&records); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "body must be a JSON array: " + err.Error()})
			return
		}

		pushed := 0
		for _, rec := range records {
			record := toUsageRecord(rec)
			usage.PublishRecord(context.Background(), record)
			pushed++
		}

		c.JSON(http.StatusOK, gin.H{"pushed": pushed, "total": len(records)})
	}
}

func toUsageRecord(rec incomingRecord) usage.Record {
	var ts time.Time
	if rec.Timestamp != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil {
			ts = parsed
		}
	}
	if ts.IsZero() {
		ts = time.Now()
	}

	var tok tokenFields
	if len(rec.Tokens) > 0 {
		_ = json.Unmarshal(rec.Tokens, &tok)
	}

	latency := time.Duration(rec.LatencyMs) * time.Millisecond

	return usage.Record{
		Provider:    rec.Provider,
		Model:       rec.Model,
		APIKey:      rec.APIKey,
		AuthIndex:   rec.AuthIndex,
		Source:      rec.Source,
		RequestedAt: ts,
		Latency:     latency,
		Failed:      rec.Failed,
		Detail: usage.Detail{
			InputTokens:     tok.InputTokens,
			OutputTokens:    tok.OutputTokens,
			ReasoningTokens: tok.ReasoningTokens,
			CachedTokens:    tok.CachedTokens,
			TotalTokens:     tok.TotalTokens,
		},
	}
}

func authenticate(c *gin.Context, cfg *config.Config) bool {
	secretKey := ""
	if cfg != nil {
		secretKey = cfg.RemoteManagement.SecretKey
	}
	if secretKey == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "remote management key not configured"})
		return false
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
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing management key"})
		return false
	}

	if subtle.ConstantTimeCompare([]byte(provided), []byte(secretKey)) == 1 {
		return true
	}
	if bcrypt.CompareHashAndPassword([]byte(secretKey), []byte(provided)) == nil {
		return true
	}

	c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid management key"})
	return false
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
