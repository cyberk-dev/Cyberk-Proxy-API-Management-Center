package ratelimit

import (
	"context"
	"fmt"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// RateLimitSelector wraps an inner coreauth.Selector and enforces per-frame
// rate limits for downstream WebSocket connections. HTTP requests are handled
// by the Gin middleware and pass through here untouched.
type RateLimitSelector struct {
	inner coreauth.Selector
	store *ConfigStore
	lim   *Limiter
}

func NewRateLimitSelector(inner coreauth.Selector, store *ConfigStore, lim *Limiter) *RateLimitSelector {
	return &RateLimitSelector{inner: inner, store: store, lim: lim}
}

func (s *RateLimitSelector) Pick(
	ctx context.Context,
	provider, model string,
	opts cliproxyexecutor.Options,
	auths []*coreauth.Auth,
) (*coreauth.Auth, error) {
	if !cliproxyexecutor.DownstreamWebsocket(ctx) {
		return s.inner.Pick(ctx, provider, model, opts, auths)
	}

	apiKey := extractAPIKeyFromHeader(opts.Headers.Get("Authorization"))
	if apiKey == "" {
		return s.inner.Pick(ctx, provider, model, opts, auths)
	}

	// Use the client-requested model from metadata, not the SDK-normalized
	// `model` arg, so rate-limit buckets match what the user actually typed.
	requestedModel := metadataModel(opts.Metadata)

	cfg := s.store.Get()
	if !cfg.Enabled() {
		return s.inner.Pick(ctx, provider, model, opts, auths)
	}

	limit, window, ok := cfg.Resolve(apiKey, requestedModel)
	if !ok {
		return s.inner.Pick(ctx, provider, model, opts, auths)
	}

	allowed, _, resetAt := s.lim.Take(apiKey, requestedModel, limit, window)
	if !allowed {
		retryAfter := int(time.Until(resetAt).Seconds())
		if retryAfter < 1 {
			retryAfter = 1
		}
		log.WithFields(log.Fields{
			"event":    "ratelimit.ws_rejected",
			"key_hash": HashKey(apiKey),
			"model":    requestedModel,
			"limit":    limit,
			"window":   window.String(),
			"retry_s":  retryAfter,
		}).Warn("rate limit exceeded (websocket frame)")

		return nil, &rateLimitError{
			model:   requestedModel,
			limit:   limit,
			window:  window,
			resetAt: resetAt,
		}
	}

	return s.inner.Pick(ctx, provider, model, opts, auths)
}

func (s *RateLimitSelector) Stop() {
	if st, ok := s.inner.(coreauth.StoppableSelector); ok {
		st.Stop()
	}
}

func extractAPIKeyFromHeader(auth string) string {
	auth = strings.TrimSpace(auth)
	if auth == "" {
		return ""
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
		return strings.TrimSpace(parts[1])
	}
	return auth
}

func metadataModel(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	v, ok := meta[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// rateLimitError returns status 400 with "invalid_request_error" in the message
// so the SDK's isRequestInvalidError returns true and shouldRetryAfterError
// returns false — the error propagates to the client without sleeping or
// retrying other auths. We use 400 instead of 429 because 429 would trigger
// the SDK's cooldown/retry-after path (conductor.go:2110-2120), defeating the
// rate limit.
type rateLimitError struct {
	model   string
	limit   int
	window  time.Duration
	resetAt time.Time
}

func (e *rateLimitError) Error() string {
	displayModel := e.model
	if displayModel == "" {
		displayModel = "*"
	}
	retryAfter := int(time.Until(e.resetAt).Seconds())
	if retryAfter < 1 {
		retryAfter = 1
	}
	return fmt.Sprintf(
		"invalid_request_error: Quota exceeded for model %q — %d req / %s. Try again in %ds or switch model.",
		displayModel, e.limit, e.window, retryAfter,
	)
}

func (e *rateLimitError) StatusCode() int {
	return 400
}
