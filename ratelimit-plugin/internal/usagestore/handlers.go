package usagestore

import (
	"crypto/subtle"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"golang.org/x/crypto/bcrypt"
)

func RegisterRoutes(engine *gin.Engine, cfg *config.Config, store *Store) {
	auth := makeAuthMiddleware(cfg)

	engine.GET("/v0/management/usage", auth, func(c *gin.Context) {
		c.JSON(http.StatusOK, store.Snapshot())
	})

	engine.GET("/v0/management/usage/summary", auth, func(c *gin.Context) {
		var since time.Time
		if sinceStr := c.Query("since"); sinceStr != "" {
			if ms, err := strconv.ParseInt(sinceStr, 10, 64); err == nil && ms > 0 {
				since = time.UnixMilli(ms)
			}
		}
		c.JSON(http.StatusOK, store.SummarySnapshot(since))
	})

	engine.GET("/v0/management/usage/keys/:key", auth, func(c *gin.Context) {
		key := c.Param("key")
		snap := store.KeySnapshot(key)
		if snap == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "api key not found in usage data"})
			return
		}
		c.JSON(http.StatusOK, snap)
	})

	engine.GET("/v0/management/usage/export", auth, func(c *gin.Context) {
		c.JSON(http.StatusOK, store.Export())
	})

	engine.POST("/v0/management/usage/import", auth, func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body: " + err.Error()})
			return
		}
		added, importErr := store.Import(body)
		if importErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid import payload: " + importErr.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"added":          added,
			"total_requests": store.Snapshot().TotalRequests,
		})
	})
}

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
