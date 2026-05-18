package contextbudget

import (
	"strings"
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

func TestParse_DisabledExplicitly(t *testing.T) {
	cfg := mustParse(t, `
context_budget:
  enabled: false
  soft_threshold_tokens: 100
  hard_threshold_tokens: 200
`)
	if cfg.Enabled() {
		t.Error("enabled:false should override threshold presence")
	}
}

func TestParse_DefaultsApplyWhenZero(t *testing.T) {
	cfg := mustParse(t, `
context_budget:
  enabled: true
`)
	if !cfg.Enabled() {
		t.Fatal("enabled:true with no thresholds should still be enabled")
	}
	if cfg.Soft() != DefaultSoftThresholdTokens {
		t.Errorf("Soft() = %d, want default %d", cfg.Soft(), DefaultSoftThresholdTokens)
	}
	if cfg.Hard() != DefaultHardThresholdTokens {
		t.Errorf("Hard() = %d, want default %d", cfg.Hard(), DefaultHardThresholdTokens)
	}
}

func TestParse_CustomThresholds(t *testing.T) {
	cfg := mustParse(t, `
context_budget:
  enabled: true
  soft_threshold_tokens: 5000
  hard_threshold_tokens: 10000
`)
	if cfg.Soft() != 5000 || cfg.Hard() != 10000 {
		t.Errorf("got soft=%d hard=%d", cfg.Soft(), cfg.Hard())
	}
}

func TestParse_ImplicitEnableViaThresholds(t *testing.T) {
	cfg := mustParse(t, `
context_budget:
  soft_threshold_tokens: 5000
`)
	if !cfg.Enabled() {
		t.Error("threshold present should implicitly enable")
	}
}

func TestParse_OtherSectionsIgnored(t *testing.T) {
	cfg := mustParse(t, `
ratelimit:
  window: 5h
policy:
  block_service_tiers: [priority]
`)
	if cfg.Enabled() {
		t.Error("unrelated sections should not enable context_budget")
	}
}

func TestReminder_TemplateSubstitution(t *testing.T) {
	cfg := mustParse(t, `
context_budget:
  enabled: true
  soft_threshold_tokens: 100
  hard_threshold_tokens: 200
  reminder_template: "used={{used}} soft={{soft}} hard={{hard}}"
`)
	got := cfg.Reminder(150)
	want := "used=150 soft=100 hard=200"
	if got != want {
		t.Errorf("Reminder = %q, want %q", got, want)
	}
}

func TestReminder_DefaultTemplateSurfacesNumbers(t *testing.T) {
	cfg := mustParse(t, `
context_budget:
  enabled: true
`)
	got := cfg.Reminder(123456)
	// The default template is intentionally generic (no tool-specific
	// jargon like /compact) so it stays useful for raw SDK callers; CLI
	// deployments override via reminder_template. It must still surface
	// the observed token count and the configured hard limit so a
	// downstream model can reason about the situation.
	if !strings.Contains(got, "123456") {
		t.Errorf("default reminder missing used count: %q", got)
	}
	if !strings.Contains(got, "summary") && !strings.Contains(got, "summarize") {
		t.Errorf("default reminder should hint at summarization, got: %q", got)
	}
}

func TestEnabled_NilConfig(t *testing.T) {
	var cfg *Config
	if cfg.Enabled() {
		t.Error("nil config should never be enabled")
	}
	if cfg.Soft() != DefaultSoftThresholdTokens {
		t.Errorf("nil Soft() = %d", cfg.Soft())
	}
}

func TestConfigStore_AtomicSwap(t *testing.T) {
	store := NewConfigStore(mustParse(t, ``))
	if store.Get().Enabled() {
		t.Fatal("initial store should be disabled")
	}
	store.Set(mustParse(t, `
context_budget:
  enabled: true
  soft_threshold_tokens: 1
  hard_threshold_tokens: 2
`))
	if !store.Get().Enabled() {
		t.Fatal("after swap should be enabled")
	}
	if store.Get().Hard() != 2 {
		t.Errorf("after swap hard=%d, want 2", store.Get().Hard())
	}
}
