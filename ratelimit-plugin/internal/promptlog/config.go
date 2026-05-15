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
)

// Config controls promptlog behavior. An absent / disabled config makes the
// middleware a pass-through.
type Config struct {
	Enabled      bool
	Dir          string
	MaxTextBytes int
	QueueSize    int
}

func (c *Config) IsEnabled() bool {
	return c != nil && c.Enabled && c.Dir != ""
}

type rawRoot struct {
	PromptLog rawConfig `yaml:"prompt_log"`
}

type rawConfig struct {
	Enabled      *bool  `yaml:"enabled"`
	Dir          string `yaml:"dir"`
	MaxTextBytes int    `yaml:"max_text_bytes"`
	QueueSize    int    `yaml:"queue_size"`
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
	return cfg, nil
}
