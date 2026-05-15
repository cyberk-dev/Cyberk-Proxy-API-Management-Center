// Package promptlog captures the user-authored portion of each chat request
// (the final user message only — earlier turns are already present in earlier
// log entries) and writes it to a daily-rotated JSONL file for offline
// analysis. Image / document payloads are masked to metadata so log files do
// not balloon with base64 blobs, and very long text blocks are middle-truncated
// so a single paste cannot dominate the log.
package promptlog

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	defaultMaxTextBytes = 50 * 1024
	defaultQueueSize    = 1024
	defaultDirName      = "prompts"

	// Template detector defaults — chosen so that a 200-character prefix
	// repeated 3+ times within the rolling window qualifies as a template.
	// MIN_LEN=200 sits well above typical short user replies ("yes", "ok",
	// "continue") so legitimate prose is not collapsed; K=3 catches PR-review
	// style slash commands almost immediately while still requiring more than
	// a single accidental retry to register.
	defaultTplMinLen     = 200
	defaultTplMinOccur   = 3
	defaultTplWindow     = 5000
	defaultTplScanEvery  = 5 * 60 // seconds
)

// Config controls promptlog behavior. An absent / disabled config makes the
// middleware a pass-through.
type Config struct {
	Enabled      bool
	Dir          string
	MaxTextBytes int
	QueueSize    int

	// Templates controls the dynamic prefix-template detector. Disabled
	// makes the writer skip templating entirely (entries store full prompts
	// as before).
	Templates TemplatesConfig
}

// TemplatesConfig tunes the dynamic detector. All fields have working
// defaults; override only when the workload demands it.
type TemplatesConfig struct {
	Enabled    bool
	MinLen     int // minimum prefix length in chars
	MinOccur   int // minimum occurrences before registration
	Window     int // rolling window size (entries kept in trie)
	ScanEvery  int // detector tick in seconds
}

func (c *Config) IsEnabled() bool {
	return c != nil && c.Enabled && c.Dir != ""
}

type rawRoot struct {
	PromptLog rawConfig `yaml:"prompt_log"`
}

type rawConfig struct {
	Enabled      *bool        `yaml:"enabled"`
	Dir          string       `yaml:"dir"`
	MaxTextBytes int          `yaml:"max_text_bytes"`
	QueueSize    int          `yaml:"queue_size"`
	Templates    rawTemplates `yaml:"templates"`
}

type rawTemplates struct {
	Enabled   *bool `yaml:"enabled"`
	MinLen    int   `yaml:"min_len"`
	MinOccur  int   `yaml:"min_occur"`
	Window    int   `yaml:"window"`
	ScanEvery int   `yaml:"scan_every_seconds"`
}

// LoadFromFile reads the prompt_log section from the proxy config file. A
// missing section returns a disabled Config (not an error) so existing
// deployments work unchanged.
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
	cfg := &Config{
		Dir:          strings.TrimSpace(root.PromptLog.Dir),
		MaxTextBytes: root.PromptLog.MaxTextBytes,
		QueueSize:    root.PromptLog.QueueSize,
	}
	// `enabled` is a pointer so users can omit it. Default: enabled when a
	// non-empty dir is supplied, otherwise disabled.
	if root.PromptLog.Enabled != nil {
		cfg.Enabled = *root.PromptLog.Enabled
	} else {
		cfg.Enabled = cfg.Dir != ""
	}
	if cfg.MaxTextBytes <= 0 {
		cfg.MaxTextBytes = defaultMaxTextBytes
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}
	rt := root.PromptLog.Templates
	cfg.Templates = TemplatesConfig{
		MinLen:    rt.MinLen,
		MinOccur:  rt.MinOccur,
		Window:    rt.Window,
		ScanEvery: rt.ScanEvery,
	}
	if rt.Enabled != nil {
		cfg.Templates.Enabled = *rt.Enabled
	} else {
		// Default ON when promptlog itself is enabled — the whole point of
		// the feature is to keep logs readable as repeated prefixes accrue.
		cfg.Templates.Enabled = cfg.Enabled
	}
	if cfg.Templates.MinLen <= 0 {
		cfg.Templates.MinLen = defaultTplMinLen
	}
	if cfg.Templates.MinOccur <= 0 {
		cfg.Templates.MinOccur = defaultTplMinOccur
	}
	if cfg.Templates.Window <= 0 {
		cfg.Templates.Window = defaultTplWindow
	}
	if cfg.Templates.ScanEvery <= 0 {
		cfg.Templates.ScanEvery = defaultTplScanEvery
	}
	return cfg, nil
}
