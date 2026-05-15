package promptlog

import "testing"

func TestParseBytes_MissingSectionDisabled(t *testing.T) {
	cfg, err := ParseBytes([]byte(`port: 8317`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IsEnabled() {
		t.Fatalf("expected disabled, got %+v", cfg)
	}
}

func TestParseBytes_ExplicitDir(t *testing.T) {
	cfg, err := ParseBytes([]byte(`
prompt_log:
  dir: /var/log/prompts
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IsEnabled() {
		t.Fatalf("expected enabled when dir set")
	}
	if cfg.Dir != "/var/log/prompts" {
		t.Errorf("dir = %q", cfg.Dir)
	}
	if cfg.MaxTextBytes != defaultMaxTextBytes {
		t.Errorf("MaxTextBytes default = %d, got %d", defaultMaxTextBytes, cfg.MaxTextBytes)
	}
	if cfg.QueueSize != defaultQueueSize {
		t.Errorf("QueueSize default = %d, got %d", defaultQueueSize, cfg.QueueSize)
	}
}

func TestParseBytes_ExplicitDisable(t *testing.T) {
	cfg, err := ParseBytes([]byte(`
prompt_log:
  enabled: false
  dir: /var/log/prompts
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IsEnabled() {
		t.Fatalf("expected disabled when enabled=false, got %+v", cfg)
	}
}

func TestParseBytes_OverrideDefaults(t *testing.T) {
	cfg, err := ParseBytes([]byte(`
prompt_log:
  dir: prompts
  max_text_bytes: 100
  queue_size: 50
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxTextBytes != 100 || cfg.QueueSize != 50 {
		t.Errorf("override failed: %+v", cfg)
	}
}
