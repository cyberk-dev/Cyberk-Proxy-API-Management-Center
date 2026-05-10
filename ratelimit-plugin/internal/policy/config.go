// Package policy enforces request-shape restrictions on top of CLIProxyAPI —
// e.g. blocking OpenAI service_tier=priority — that are independent of the
// per-key rate-limit accounting in internal/ratelimit.
package policy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// Config captures the policy: section of config.yaml. Empty / missing section
// → Enabled() returns false → middleware is a pass-through.
type Config struct {
	BlockServiceTiers []string
}

func (c *Config) Enabled() bool {
	return c != nil && len(c.BlockServiceTiers) > 0
}

// IsBlockedTier returns true when tier (case-insensitive) is in the blocklist.
// Empty tier never blocks — clients that omit the field always pass through.
func (c *Config) IsBlockedTier(tier string) bool {
	if !c.Enabled() {
		return false
	}
	t := strings.ToLower(strings.TrimSpace(tier))
	if t == "" {
		return false
	}
	for _, blocked := range c.BlockServiceTiers {
		if strings.ToLower(strings.TrimSpace(blocked)) == t {
			return true
		}
	}
	return false
}

type rawRoot struct {
	Policy rawConfig `yaml:"policy"`
}

type rawConfig struct {
	BlockServiceTiers []string `yaml:"block_service_tiers"`
}

func LoadFromFile(p string) (*Config, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return ParseBytes(data)
}

func ParseBytes(data []byte) (*Config, error) {
	var root rawRoot
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("unmarshal yaml: %w", err)
	}
	cfg := &Config{}
	for _, t := range root.Policy.BlockServiceTiers {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		cfg.BlockServiceTiers = append(cfg.BlockServiceTiers, t)
	}
	return cfg, nil
}

// ConfigStore holds an atomic.Pointer[Config] so middleware can read a
// consistent snapshot without locking. Mirrors ratelimit.ConfigStore but kept
// separate to avoid coupling the two packages' lifecycles.
type ConfigStore struct {
	ptr atomic.Pointer[Config]
}

func NewConfigStore(cfg *Config) *ConfigStore {
	s := &ConfigStore{}
	s.Set(cfg)
	return s
}

func (s *ConfigStore) Get() *Config {
	if s == nil {
		return nil
	}
	return s.ptr.Load()
}

func (s *ConfigStore) Set(cfg *Config) {
	if s == nil {
		return
	}
	s.ptr.Store(cfg)
}

// Watch reloads the policy section whenever the config file changes. The
// fsnotify watcher targets the parent directory (atomic-write friendly,
// K8s-ConfigMap compatible) and falls back to a 30s mtime poll for backends
// that miss events (Docker Desktop VirtioFS).
func (s *ConfigStore) Watch(ctx context.Context, path string, onReload func(*Config)) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(filepath.Dir(path)); err != nil {
		_ = w.Close()
		return err
	}

	go func() {
		defer func() { _ = w.Close() }()

		debounce := time.NewTimer(time.Hour)
		if !debounce.Stop() {
			<-debounce.C
		}

		fallback := time.NewTicker(30 * time.Second)
		defer fallback.Stop()

		var lastMtime time.Time
		if info, err := os.Stat(path); err == nil {
			lastMtime = info.ModTime()
		}

		reload := func() {
			info, err := os.Stat(path)
			if err != nil {
				log.Warnf("policy: stat config: %v", err)
				return
			}
			if info.ModTime().Equal(lastMtime) {
				return
			}

			cfg, err := LoadFromFile(path)
			if err != nil {
				// Don't advance lastMtime on parse failure — see ratelimit/store.go
				// for the rationale (partial writes during atomic rename / K8s
				// ConfigMap update need to be retried).
				log.Warnf("policy: reload config: %v", err)
				return
			}
			lastMtime = info.ModTime()
			s.Set(cfg)
			if onReload != nil {
				onReload(cfg)
			}
			log.Infof("policy: config reloaded (mtime=%s)", info.ModTime().Format(time.RFC3339))
		}

		trigger := func() {
			if !debounce.Stop() {
				select {
				case <-debounce.C:
				default:
				}
			}
			debounce.Reset(500 * time.Millisecond)
		}

		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-w.Events:
				if !ok {
					return
				}
				trigger()
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Warnf("policy: fsnotify error: %v", err)
			case <-debounce.C:
				reload()
			case <-fallback.C:
				reload()
			}
		}
	}()
	return nil
}
