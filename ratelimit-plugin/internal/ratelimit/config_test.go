package ratelimit

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, yaml string) *Config {
	t.Helper()
	cfg, err := ParseBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

func TestParse_TopLevelOnly(t *testing.T) {
	cfg := mustParse(t, `
ratelimit:
  window: 5h
  requests: 500
`)
	if cfg.Window != 5*time.Hour {
		t.Errorf("window: got %v", cfg.Window)
	}
	if cfg.Requests != 500 {
		t.Errorf("requests: got %d", cfg.Requests)
	}
	if !cfg.Enabled() {
		t.Error("should be enabled")
	}
}

func TestParse_ModelOverrides(t *testing.T) {
	cfg := mustParse(t, `
ratelimit:
  window: 5h
  requests: 500
  models:
    gpt-4:
      window: 2h
      requests: 100
      keys:
        vip: 50
    gpt-mini:
      requests: 300
`)
	gpt4 := cfg.Models["gpt-4"]
	if gpt4.Window == nil || *gpt4.Window != 2*time.Hour {
		t.Errorf("gpt-4 window: got %v", gpt4.Window)
	}
	if gpt4.Requests == nil || *gpt4.Requests != 100 {
		t.Errorf("gpt-4 requests: got %v", gpt4.Requests)
	}
	if gpt4.Keys["vip"] != 50 {
		t.Errorf("vip override: got %d", gpt4.Keys["vip"])
	}
	mini := cfg.Models["gpt-mini"]
	if mini.Window != nil {
		t.Errorf("gpt-mini should inherit window, got %v", mini.Window)
	}
}

func TestParse_Invalid(t *testing.T) {
	cases := []string{
		`ratelimit: { window: "not-a-duration", requests: 10 }`,
		`ratelimit: { window: 5h, requests: -1 }`,
		`ratelimit: { window: -1h, requests: 10 }`,
		`ratelimit: { models: { foo: { requests: -5 } } }`,
	}
	for _, c := range cases {
		if _, err := ParseBytes([]byte(c)); err == nil {
			t.Errorf("expected error for: %s", c)
		}
	}
}

func TestParse_Empty(t *testing.T) {
	cfg, err := ParseBytes([]byte(`other: stuff`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled() {
		t.Error("empty config should not be enabled")
	}
}

func TestResolve_Precedence(t *testing.T) {
	cfg := mustParse(t, `
ratelimit:
  window: 5h
  requests: 500
  models:
    gpt-4:
      window: 2h
      requests: 100
      keys:
        vip: 50
    "gpt-4-*":
      requests: 300
    "gpt-*":
      requests: 200
`)

	cases := []struct {
		name        string
		key, model  string
		wantLimit   int
		wantWindow  time.Duration
		wantApplies bool
	}{
		{"exact model + per-key", "vip", "gpt-4", 50, 2 * time.Hour, true},
		{"exact model default", "alice", "gpt-4", 100, 2 * time.Hour, true},
		{"wildcard more-specific beats less-specific", "alice", "gpt-4-mini", 300, 5 * time.Hour, true},
		{"less-specific wildcard fallback", "alice", "gpt-3.5", 200, 5 * time.Hour, true},
		{"top-level default", "alice", "claude-3", 500, 5 * time.Hour, true},
		{"no model → top-level default", "alice", "", 500, 5 * time.Hour, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lim, win, ok := cfg.Resolve(tc.key, tc.model)
			if ok != tc.wantApplies || lim != tc.wantLimit || win != tc.wantWindow {
				t.Errorf("got (%d, %v, %v), want (%d, %v, %v)",
					lim, win, ok, tc.wantLimit, tc.wantWindow, tc.wantApplies)
			}
		})
	}
}

func TestResolve_ExactBeatsWildcard(t *testing.T) {
	cfg := mustParse(t, `
ratelimit:
  window: 5h
  models:
    "gpt-4":
      requests: 100
    "gpt-*":
      requests: 999
`)
	lim, _, _ := cfg.Resolve("alice", "gpt-4")
	if lim != 100 {
		t.Errorf("exact should win, got %d", lim)
	}
}

func TestResolve_CacheInvariant(t *testing.T) {
	cfg := mustParse(t, `ratelimit: { window: 1h, requests: 10 }`)
	for i := 0; i < 5; i++ {
		lim, _, _ := cfg.Resolve("k", "m")
		if lim != 10 {
			t.Errorf("iter %d: got %d", i, lim)
		}
	}
}

func TestResolve_NoLimit(t *testing.T) {
	cfg := mustParse(t, `ratelimit: {}`)
	_, _, ok := cfg.Resolve("any", "any")
	if ok {
		t.Error("empty config should not apply limit")
	}
}

func TestParse_AliasCanonical(t *testing.T) {
	cfg := mustParse(t, `
ratelimit:
  window: 5h
  requests: 100
oauth-model-alias:
  codex:
    - name: gpt-5.5
      alias: gpt-5.5-high
      fork: true
    - name: gpt-5.5
      alias: claude-opus-4-8
      fork: true
    - name: gpt-5.4
      alias: claude-sonnet-4-6
      fork: true
  kiro:
    - name: kiro-claude-sonnet-4-6
      alias: claude-sonnet-4-6
      fork: true
`)

	cases := []struct {
		name, in, want string
	}{
		{"unambiguous fork alias", "gpt-5.5-high", "gpt-5.5"},
		{"unambiguous claude alias → gpt-5.5", "claude-opus-4-8", "gpt-5.5"},
		{"case-insensitive", "CLAUDE-OPUS-4-8", "gpt-5.5"},
		{"trimmed", "  claude-opus-4-8  ", "gpt-5.5"},
		{"ambiguous across providers left alone", "claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"upstream name passes through", "gpt-5.5", "gpt-5.5"},
		{"unknown model normalized passthrough", "  GPT-4o ", "gpt-4o"},
		{"empty", "", ""},
		// Thinking suffix "(value)" is stripped before the alias lookup so it
		// collapses onto the upstream counter instead of dodging the cap.
		{"alias with level suffix", "claude-opus-4-8(high)", "gpt-5.5"},
		{"alias with numeric suffix", "claude-opus-4-8(8192)", "gpt-5.5"},
		{"upstream name with suffix collapses to base", "gpt-5.5(high)", "gpt-5.5"},
		{"suffix is case-insensitive too", "CLAUDE-OPUS-4-8(HIGH)", "gpt-5.5"},
		{"unbalanced open paren left as-is", "gpt-5.5(high", "gpt-5.5(high"},
		{"closing paren without open left as-is", "gpt-5.5high)", "gpt-5.5high)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cfg.Canonical(tc.in); got != tc.want {
				t.Errorf("Canonical(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCanonical_NilAndNoAliasSection(t *testing.T) {
	var nilCfg *Config
	if got := nilCfg.Canonical("Foo"); got != "foo" {
		t.Errorf("nil config: got %q, want foo", got)
	}
	cfg := mustParse(t, `ratelimit: { window: 1h, requests: 5 }`)
	if cfg.aliasCanonical != nil {
		t.Errorf("no alias section should yield nil map, got %v", cfg.aliasCanonical)
	}
	if got := cfg.Canonical("bar"); got != "bar" {
		t.Errorf("no alias section: got %q, want bar", got)
	}
}

func TestResolve_ZeroKeyOverrideSkipped(t *testing.T) {
	cfg := mustParse(t, `
ratelimit:
  window: 5h
  requests: 500
  models:
    gpt-4:
      requests: 100
      keys:
        disabled: 0
`)
	// 0 key override → không apply → fall through to model default
	lim, _, _ := cfg.Resolve("disabled", "gpt-4")
	if lim != 100 {
		t.Errorf("expected model default 100, got %d", lim)
	}
}
