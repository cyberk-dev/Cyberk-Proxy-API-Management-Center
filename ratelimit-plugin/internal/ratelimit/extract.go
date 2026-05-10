package ratelimit

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// maxBodyPeek caps how much of a JSON request body we buffer to extract
// fields like "model" or "service_tier". Set high enough to cover realistic
// large multi-turn conversations from the OpenAI Python SDK (which serializes
// `messages` before `service_tier`), at the cost of up to maxBodyPeek bytes
// of memory per concurrent JSON request. Bodies larger than this are still
// forwarded in full — only the policy/limit decisions are made from the
// prefix, and PeekResult.Truncated lets callers warn when the cap is hit.
const maxBodyPeek = 4 << 20

// peekCacheKey is the gin.Context key under which body-peek results are
// cached so multiple middlewares (ratelimit, policy) share one read.
const peekCacheKey = "ratelimit.peek_body"

// PeekResult is the cached outcome of one PeekJSONBody attempt.
type PeekResult struct {
	// Body is the buffered prefix (up to maxBodyPeek bytes). Nil if the
	// request wasn't JSON, had no body, or read failed.
	Body []byte
	// Truncated is true when the peek hit the maxBodyPeek cap — fields
	// appearing later in the body will not be visible to gjson lookups.
	Truncated bool
}

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

	peek := PeekJSONBody(c)
	if len(peek) == 0 {
		return ""
	}
	m := gjson.GetBytes(peek, "model").String()
	return strings.ToLower(strings.TrimSpace(m))
}

// PeekJSONBody reads up to maxBodyPeek bytes from a JSON request body, resets
// the request body so downstream handlers see the original bytes, and caches
// the peek in c so subsequent calls within the same request are O(1). Returns
// nil if the request isn't JSON, has no body, or read failed — callers must
// treat empty as "field not present".
func PeekJSONBody(c *gin.Context) []byte {
	return PeekJSONBodyResult(c).Body
}

// PeekJSONBodyResult is the truncation-aware version of PeekJSONBody. Callers
// that need to know whether the peek hit the maxBodyPeek cap (e.g. to log a
// warning that policy decisions may be incomplete) should use this.
func PeekJSONBodyResult(c *gin.Context) PeekResult {
	if c == nil || c.Request == nil {
		return PeekResult{}
	}

	if v, ok := c.Get(peekCacheKey); ok {
		if r, ok := v.(PeekResult); ok {
			return r
		}
	}

	store := func(r PeekResult) PeekResult {
		c.Set(peekCacheKey, r)
		return r
	}

	ct := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
	if semi := strings.Index(ct, ";"); semi >= 0 {
		ct = strings.TrimSpace(ct[:semi])
	}
	if ct != "application/json" {
		return store(PeekResult{})
	}
	if c.Request.Body == nil {
		return store(PeekResult{})
	}

	limited := io.LimitReader(c.Request.Body, maxBodyPeek)
	peek, err := io.ReadAll(limited)
	if err != nil {
		c.Request.Body = io.NopCloser(bytes.NewReader(peek))
		return store(PeekResult{})
	}

	truncated := int64(len(peek)) >= maxBodyPeek
	if !truncated {
		c.Request.Body = io.NopCloser(bytes.NewReader(peek))
	} else {
		orig := c.Request.Body
		c.Request.Body = &multiReadCloser{
			Reader: io.MultiReader(bytes.NewReader(peek), orig),
			Closer: orig,
		}
	}

	if len(peek) == 0 {
		return store(PeekResult{})
	}
	return store(PeekResult{Body: peek, Truncated: truncated})
}

type multiReadCloser struct {
	io.Reader
	io.Closer
}
