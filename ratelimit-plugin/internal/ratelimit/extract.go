package ratelimit

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const maxBodyPeek = 1 << 20

func ExtractAPIKey(r *http.Request) string {
	if r == nil {
		return ""
	}
	if h := strings.TrimSpace(r.Header.Get("Authorization")); h != "" {
		parts := strings.SplitN(h, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			return strings.TrimSpace(parts[1])
		}
		return h
	}
	if h := strings.TrimSpace(r.Header.Get("X-Goog-Api-Key")); h != "" {
		return h
	}
	if h := strings.TrimSpace(r.Header.Get("X-Api-Key")); h != "" {
		return h
	}
	if r.URL != nil {
		if k := r.URL.Query().Get("key"); k != "" {
			return k
		}
		if k := r.URL.Query().Get("auth_token"); k != "" {
			return k
		}
	}
	return ""
}

func ExtractModel(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	path := c.Request.URL.Path

	if strings.HasPrefix(path, "/v1beta/models/") {
		rest := strings.TrimPrefix(path, "/v1beta/models/")
		if rest == "" {
			return ""
		}
		if i := strings.Index(rest, ":"); i >= 0 {
			return strings.ToLower(rest[:i])
		}
		if i := strings.Index(rest, "/"); i >= 0 {
			return strings.ToLower(rest[:i])
		}
		return strings.ToLower(rest)
	}

	ct := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
	if ct != "" {
		if semi := strings.Index(ct, ";"); semi >= 0 {
			ct = strings.TrimSpace(ct[:semi])
		}
	}
	if ct != "application/json" {
		return ""
	}
	if c.Request.Body == nil {
		return ""
	}

	limited := io.LimitReader(c.Request.Body, maxBodyPeek)
	peek, err := io.ReadAll(limited)
	if err != nil {
		c.Request.Body = io.NopCloser(bytes.NewReader(peek))
		return ""
	}

	if int64(len(peek)) < maxBodyPeek {
		c.Request.Body = io.NopCloser(bytes.NewReader(peek))
	} else {
		orig := c.Request.Body
		c.Request.Body = &multiReadCloser{
			Reader: io.MultiReader(bytes.NewReader(peek), orig),
			Closer: orig,
		}
	}

	m := gjson.GetBytes(peek, "model").String()
	return strings.ToLower(strings.TrimSpace(m))
}

type multiReadCloser struct {
	io.Reader
	io.Closer
}
