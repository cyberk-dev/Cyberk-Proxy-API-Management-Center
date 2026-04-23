package ratelimit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigStore_GetSet(t *testing.T) {
	store := NewConfigStore(nil)
	if store.Get() != nil {
		t.Error("initial should be nil")
	}
	cfg, _ := ParseBytes([]byte(`ratelimit: { window: 1h, requests: 10 }`))
	store.Set(cfg)
	if got := store.Get(); got == nil || got.Requests != 10 {
		t.Errorf("after set: %+v", got)
	}
}

func TestConfigStore_HotReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`ratelimit: { window: 1h, requests: 10 }`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store := NewConfigStore(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reloaded := make(chan *Config, 4)
	if err := store.Watch(ctx, path, func(c *Config) { reloaded <- c }); err != nil {
		t.Fatal(err)
	}

	// Sleep briefly to ensure mtime differs (some filesystems have 1s resolution).
	time.Sleep(1100 * time.Millisecond)
	if err := os.WriteFile(path, []byte(`ratelimit: { window: 1h, requests: 99 }`), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case c := <-reloaded:
		if c.Requests != 99 {
			t.Errorf("reload got requests=%d, want 99", c.Requests)
		}
		if got := store.Get().Requests; got != 99 {
			t.Errorf("store returns stale: %d", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reload timeout")
	}
}

func TestConfigStore_WatchReloadsOnInvalidIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`ratelimit: { window: 1h, requests: 10 }`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _ := LoadFromFile(path)
	store := NewConfigStore(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = store.Watch(ctx, path, nil)

	time.Sleep(1100 * time.Millisecond)
	// Write an invalid config — old config should remain.
	_ = os.WriteFile(path, []byte(`ratelimit: { window: "not-a-duration", requests: 10 }`), 0o644)

	time.Sleep(1 * time.Second)

	if got := store.Get().Requests; got != 10 {
		t.Errorf("invalid reload should keep old config, got requests=%d", got)
	}
}
