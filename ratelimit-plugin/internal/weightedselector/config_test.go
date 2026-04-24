package weightedselector

import "testing"

func TestLoadFromYAMLAbsent(t *testing.T) {
	// No codex_weights key -> feature disabled, zero weights map.
	cfg, err := parseBytes([]byte(`
ratelimit:
  window: 1m
  requests: 100
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Enabled {
		t.Fatalf("Enabled=true with no codex_weights block")
	}
	if cfg.WeightFor("pro") != 0 {
		t.Fatalf("disabled WeightFor should return 0, got %d", cfg.WeightFor("pro"))
	}
}

func TestLoadFromYAMLPresent(t *testing.T) {
	cfg, err := parseBytes([]byte(`
codex_weights:
  pro: 20
  plus: 1
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Enabled {
		t.Fatalf("Enabled should be true")
	}
	// Explicit override wins.
	if w := cfg.WeightFor("pro"); w != 20 {
		t.Fatalf("pro weight = %d, want 20", w)
	}
	if w := cfg.WeightFor("plus"); w != 1 {
		t.Fatalf("plus weight = %d, want 1", w)
	}
	// Defaults preserved for unlisted plans.
	if w := cfg.WeightFor("prolite"); w != 5 {
		t.Fatalf("prolite weight = %d, want default 5", w)
	}
	if w := cfg.WeightFor("team"); w != 1 {
		t.Fatalf("team weight = %d, want default 1", w)
	}
}

func TestWeightForNormalization(t *testing.T) {
	cfg, _ := parseBytes([]byte(`
codex_weights:
  pro-lite: 7
`))
	// pro-lite, pro_lite, prolite, PROLITE, " ProLite " all resolve the same.
	inputs := []string{"pro-lite", "pro_lite", "prolite", "PROLITE", " ProLite "}
	for _, in := range inputs {
		if w := cfg.WeightFor(in); w != 7 {
			t.Fatalf("WeightFor(%q) = %d, want 7", in, w)
		}
	}
}

func TestWeightForUnknownPlanFallsBackToOne(t *testing.T) {
	cfg, _ := parseBytes([]byte(`
codex_weights:
  pro: 10
`))
	// New tier not in defaults, not in override -> fall back to 1 (don't drop traffic).
	if w := cfg.WeightFor("hypothetical_new_tier"); w != defaultFallbackWeight {
		t.Fatalf("unknown plan weight = %d, want %d", w, defaultFallbackWeight)
	}
	if w := cfg.WeightFor(""); w != defaultFallbackWeight {
		t.Fatalf("empty plan weight = %d, want %d", w, defaultFallbackWeight)
	}
}

func TestLoadFromYAMLRejectsNegative(t *testing.T) {
	_, err := parseBytes([]byte(`
codex_weights:
  pro: -3
`))
	if err == nil {
		t.Fatalf("expected error for negative weight")
	}
}

func TestLoadFromYAMLEmptyBlockEnablesDefaults(t *testing.T) {
	// codex_weights: {}  -> enabled, all defaults active.
	cfg, err := parseBytes([]byte(`
codex_weights: {}
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Enabled {
		t.Fatalf("empty block should enable feature")
	}
	if w := cfg.WeightFor("pro"); w != 10 {
		t.Fatalf("pro default = %d, want 10", w)
	}
}
