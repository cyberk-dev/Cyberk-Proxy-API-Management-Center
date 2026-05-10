package policy

import (
	"testing"
)

func mustParse(t *testing.T, yaml string) *Config {
	t.Helper()
	cfg, err := ParseBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

func TestParse_Empty(t *testing.T) {
	cfg := mustParse(t, ``)
	if cfg.Enabled() {
		t.Error("empty config should not be enabled")
	}
}

func TestParse_OtherSectionsIgnored(t *testing.T) {
	cfg := mustParse(t, `
ratelimit:
  window: 5h
  requests: 500
codex_weights:
  pro: 10
`)
	if cfg.Enabled() {
		t.Error("config without policy section should not be enabled")
	}
}

func TestParse_BlockServiceTiers(t *testing.T) {
	cfg := mustParse(t, `
policy:
  block_service_tiers:
    - priority
    - flex
`)
	if !cfg.Enabled() {
		t.Fatal("should be enabled")
	}
	if len(cfg.BlockServiceTiers) != 2 {
		t.Errorf("expected 2 tiers, got %d", len(cfg.BlockServiceTiers))
	}
}

func TestParse_TrimsAndDropsEmpty(t *testing.T) {
	cfg := mustParse(t, `
policy:
  block_service_tiers:
    - "  priority  "
    - ""
    - flex
`)
	if len(cfg.BlockServiceTiers) != 2 {
		t.Errorf("expected 2 non-empty tiers, got %v", cfg.BlockServiceTiers)
	}
	if cfg.BlockServiceTiers[0] != "priority" {
		t.Errorf("expected trimmed value, got %q", cfg.BlockServiceTiers[0])
	}
}

func TestIsBlockedTier_CaseInsensitive(t *testing.T) {
	cfg := mustParse(t, `
policy:
  block_service_tiers: [priority]
`)
	for _, in := range []string{"priority", "PRIORITY", "Priority", "  priority  "} {
		if !cfg.IsBlockedTier(in) {
			t.Errorf("expected %q to be blocked", in)
		}
	}
	for _, in := range []string{"flex", "default", "auto", ""} {
		if cfg.IsBlockedTier(in) {
			t.Errorf("expected %q to be allowed", in)
		}
	}
}

func TestIsBlockedTier_DisabledConfigAllowsAll(t *testing.T) {
	cfg := mustParse(t, ``)
	if cfg.IsBlockedTier("priority") {
		t.Error("disabled config should never block")
	}
}

func TestIsBlockedTier_NilConfig(t *testing.T) {
	var cfg *Config
	if cfg.IsBlockedTier("priority") {
		t.Error("nil config should never block")
	}
	if cfg.Enabled() {
		t.Error("nil config should not be enabled")
	}
}
