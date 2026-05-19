// Package contextbudget enforces soft and hard request-size limits on top of
// CLIProxyAPI to prevent runaway context growth in long-running agent sessions.
//
// Soft limit: when the estimated input token count crosses
// SoftThresholdTokens, the middleware injects a <system-reminder> block into
// the last user message so the model is prompted to wrap up / suggest /compact
// before the conversation hits the hard ceiling.
//
// Hard limit: when the count crosses HardThresholdTokens, the request is
// rejected with HTTP 413 (non-streaming) or a single SSE error event
// (streaming) so the client can abort gracefully instead of hitting the
// upstream provider's own context-window error mid-stream.
//
// Token counts are char-based estimates (chars / 4) — accurate to within
// ~20% for English, off for code and CJK. Thresholds should have headroom.
package contextbudget

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

// Sentinel defaults used when the config section is missing or partially set.
// Picked to leave headroom for Anthropic's 200k window and OpenAI's 400k+
// windows; tune per-deployment via config.yaml.
const (
	DefaultSoftThresholdTokens = 300_000
	DefaultHardThresholdTokens = 360_000
	// Default template intentionally avoids tool-specific commands like
	// /compact so it stays useful for raw SDK callers, scripts, and agentic
	// workflows that don't have a slash-command UI. CLI deployments
	// (Claude Code, Codex CLI, opencode, gemini-cli) should override this
	// in config.yaml with /compact-aware wording.
	DefaultReminderTemplate    = "Context usage is approaching the proxy's hard limit (~{{used}} of {{hard}} tokens; soft warning fires at {{soft}}). Please respond concisely. If the task is unfinished, end your reply with a brief summary of current progress, key decisions, and next steps so the conversation can be safely truncated or restarted without losing state."
)

// Config captures the context_budget: section of config.yaml. Empty / missing
// section -> Enabled() returns false -> middleware is a pass-through.
type Config struct {
	IsEnabled            bool
	SoftThresholdTokens  int
	HardThresholdTokens  int
	ReminderTemplate     string
	// SoftBlockBurstSeconds is how long, after a session first crosses the
	// soft threshold, every subsequent request is also 400'd. The window
	// blankets the entire client retry storm (Claude Code's ~1s/2s/4s
	// backoff plus parallel sidecar requests) so the error text reliably
	// surfaces to the user. After the window expires the session passes
	// through until tokens fall back below soft (which re-arms a fresh
	// window). 0 -> use SoftBlockBurst().
	SoftBlockBurstSeconds int
}

// DefaultSoftBlockBurst is the default burst-window length used when the
// config doesn't override it. 5 s comfortably covers Claude Code's 3-attempt
// 1s/2s/4s exponential backoff with a small margin; other clients (Codex
// CLI, Amp, custom SDKs) with longer backoff schedules can raise it via
// `soft_block_burst_seconds` in config.yaml.
const DefaultSoftBlockBurst = 5 * time.Second

// Enabled reports whether the middleware should run. A nil or
// explicitly-disabled config is pass-through; an enabled config with no
// thresholds falls back to package defaults so operators get a sensible
// safety net just by adding `enabled: true`.
func (c *Config) Enabled() bool {
	if c == nil {
		return false
	}
	return c.IsEnabled
}

// Soft returns the configured soft threshold or the package default if unset.
func (c *Config) Soft() int {
	if c == nil || c.SoftThresholdTokens <= 0 {
		return DefaultSoftThresholdTokens
	}
	return c.SoftThresholdTokens
}

// Hard returns the configured hard threshold or the package default if unset.
func (c *Config) Hard() int {
	if c == nil || c.HardThresholdTokens <= 0 {
		return DefaultHardThresholdTokens
	}
	return c.HardThresholdTokens
}

// SoftBlockBurst returns the configured soft-block burst window or the
// package default if unset. main.go applies this to the Tracker.
func (c *Config) SoftBlockBurst() time.Duration {
	if c == nil || c.SoftBlockBurstSeconds <= 0 {
		return DefaultSoftBlockBurst
	}
	return time.Duration(c.SoftBlockBurstSeconds) * time.Second
}

// Reminder returns the reminder template after macro substitution. The
// template may contain {{used}}, {{soft}}, {{hard}} placeholders.
func (c *Config) Reminder(used int) string {
	tmpl := DefaultReminderTemplate
	if c != nil && c.ReminderTemplate != "" {
		tmpl = c.ReminderTemplate
	}
	r := strings.NewReplacer(
		"{{used}}", fmt.Sprintf("%d", used),
		"{{soft}}", fmt.Sprintf("%d", c.Soft()),
		"{{hard}}", fmt.Sprintf("%d", c.Hard()),
	)
	return r.Replace(tmpl)
}

type rawRoot struct {
	ContextBudget rawConfig `yaml:"context_budget"`
}

type rawConfig struct {
	Enabled               *bool  `yaml:"enabled"`
	SoftThresholdTokens   int    `yaml:"soft_threshold_tokens"`
	HardThresholdTokens   int    `yaml:"hard_threshold_tokens"`
	ReminderTemplate      string `yaml:"reminder_template"`
	SoftBlockBurstSeconds int    `yaml:"soft_block_burst_seconds"`
}

// LoadFromFile reads p and extracts the context_budget section. A missing
// file is an error; a missing section yields a disabled Config.
func LoadFromFile(p string) (*Config, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return ParseBytes(data)
}

// ParseBytes parses raw YAML and extracts the context_budget section.
func ParseBytes(data []byte) (*Config, error) {
	var root rawRoot
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("unmarshal yaml: %w", err)
	}
	r := root.ContextBudget
	cfg := &Config{
		SoftThresholdTokens:   r.SoftThresholdTokens,
		HardThresholdTokens:   r.HardThresholdTokens,
		ReminderTemplate:      r.ReminderTemplate,
		SoftBlockBurstSeconds: r.SoftBlockBurstSeconds,
	}
	if r.Enabled != nil {
		cfg.IsEnabled = *r.Enabled
	} else {
		// Default to enabled if any threshold is explicitly set; otherwise
		// disabled so dropping an empty section into config doesn't surprise
		// operators.
		cfg.IsEnabled = r.SoftThresholdTokens > 0 || r.HardThresholdTokens > 0
	}
	return cfg, nil
}

// ConfigStore holds an atomic.Pointer[Config] so middleware reads a consistent
// snapshot without locking. Mirrors ratelimit.ConfigStore / policy.ConfigStore
// but kept separate to avoid coupling lifecycles.
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

// Watch reloads the context_budget section whenever the config file changes.
// The fsnotify watcher targets the parent directory (atomic-write friendly,
// K8s-ConfigMap compatible) and falls back to a 30s mtime poll for backends
// that miss events (Docker Desktop VirtioFS). Mirrors policy.ConfigStore.Watch.
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
				log.Warnf("context_budget: stat config: %v", err)
				return
			}
			if info.ModTime().Equal(lastMtime) {
				return
			}

			cfg, err := LoadFromFile(path)
			if err != nil {
				// Don't advance lastMtime on parse failure — partial writes
				// during atomic rename / K8s ConfigMap update need retry.
				log.Warnf("context_budget: reload config: %v", err)
				return
			}
			lastMtime = info.ModTime()
			s.Set(cfg)
			if onReload != nil {
				onReload(cfg)
			}
			log.Infof("context_budget: config reloaded (mtime=%s)", info.ModTime().Format(time.RFC3339))
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
				log.Warnf("context_budget: fsnotify error: %v", err)
			case <-debounce.C:
				reload()
			case <-fallback.C:
				reload()
			}
		}
	}()
	return nil
}
