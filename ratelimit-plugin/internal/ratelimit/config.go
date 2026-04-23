package ratelimit

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Window   time.Duration
	Requests int
	Models   map[string]ModelConfig

	modelOrder []string
	resolveCache sync.Map
}

type ModelConfig struct {
	Window   *time.Duration
	Requests *int
	Keys     map[string]int
}

type resolved struct {
	limit   int
	window  time.Duration
	applies bool
}

func (c *Config) Resolve(apiKey, model string) (limit int, window time.Duration, applies bool) {
	if c == nil {
		return 0, 0, false
	}
	cacheKey := apiKey + "\x00" + model
	if v, ok := c.resolveCache.Load(cacheKey); ok {
		r := v.(resolved)
		return r.limit, r.window, r.applies
	}
	l, w, a := c.resolveUncached(apiKey, model)
	c.resolveCache.Store(cacheKey, resolved{l, w, a})
	return l, w, a
}

func (c *Config) resolveUncached(apiKey, model string) (int, time.Duration, bool) {
	normModel := strings.ToLower(strings.TrimSpace(model))

	exact, hasExact := c.Models[normModel]
	wildcardName, wildcardCfg, hasWild := c.findWildcard(normModel)

	// 1. Exact model + per-key
	if hasExact {
		if lim, ok := exact.Keys[apiKey]; ok && lim > 0 {
			return lim, derefDuration(exact.Window, c.Window), true
		}
	}
	// 2. Wildcard model + per-key
	if hasWild {
		if lim, ok := wildcardCfg.Keys[apiKey]; ok && lim > 0 {
			return lim, derefDuration(wildcardCfg.Window, c.Window), true
		}
	}
	// 3. Exact model default
	if hasExact && exact.Requests != nil && *exact.Requests > 0 {
		return *exact.Requests, derefDuration(exact.Window, c.Window), true
	}
	// 4. Wildcard default
	if hasWild && wildcardCfg.Requests != nil && *wildcardCfg.Requests > 0 {
		return *wildcardCfg.Requests, derefDuration(wildcardCfg.Window, c.Window), true
	}
	_ = wildcardName
	// 5. Top-level default
	if c.Requests > 0 && c.Window > 0 {
		return c.Requests, c.Window, true
	}
	return 0, 0, false
}

func (c *Config) findWildcard(model string) (string, ModelConfig, bool) {
	if model == "" {
		return "", ModelConfig{}, false
	}
	var bestName string
	var best ModelConfig
	bestScore := -1
	for _, name := range c.modelOrder {
		if !strings.ContainsAny(name, "*?[") {
			continue
		}
		m, err := path.Match(name, model)
		if err != nil || !m {
			continue
		}
		score := literalCharCount(name)
		if score > bestScore || (score == bestScore && name < bestName) {
			bestScore = score
			bestName = name
			best = c.Models[name]
		}
	}
	if bestScore < 0 {
		return "", ModelConfig{}, false
	}
	return bestName, best, true
}

func literalCharCount(pattern string) int {
	n := 0
	for _, r := range pattern {
		if r != '*' && r != '?' && r != '[' && r != ']' {
			n++
		}
	}
	return n
}

func derefDuration(p *time.Duration, fallback time.Duration) time.Duration {
	if p != nil && *p > 0 {
		return *p
	}
	return fallback
}

type rawRoot struct {
	Ratelimit rawConfig `yaml:"ratelimit"`
}

type rawConfig struct {
	Window   string                  `yaml:"window"`
	Requests int                     `yaml:"requests"`
	Models   map[string]rawModelConf `yaml:"models"`
}

type rawModelConf struct {
	Window   string         `yaml:"window"`
	Requests *int           `yaml:"requests"`
	Keys     map[string]int `yaml:"keys"`
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
	return buildConfig(root.Ratelimit)
}

func buildConfig(raw rawConfig) (*Config, error) {
	cfg := &Config{
		Models: map[string]ModelConfig{},
	}
	if raw.Window != "" {
		d, err := time.ParseDuration(raw.Window)
		if err != nil {
			return nil, fmt.Errorf("invalid top-level window %q: %w", raw.Window, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("top-level window must be positive, got %v", d)
		}
		cfg.Window = d
	}
	if raw.Requests < 0 {
		return nil, fmt.Errorf("top-level requests must be >= 0, got %d", raw.Requests)
	}
	cfg.Requests = raw.Requests

	names := make([]string, 0, len(raw.Models))
	for name := range raw.Models {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		m := raw.Models[name]
		normName := strings.ToLower(strings.TrimSpace(name))
		if normName == "" {
			continue
		}
		mc := ModelConfig{
			Keys: map[string]int{},
		}
		if m.Window != "" {
			d, err := time.ParseDuration(m.Window)
			if err != nil {
				return nil, fmt.Errorf("model %q invalid window %q: %w", name, m.Window, err)
			}
			if d <= 0 {
				return nil, fmt.Errorf("model %q window must be positive", name)
			}
			mc.Window = &d
		}
		if m.Requests != nil {
			if *m.Requests < 0 {
				return nil, fmt.Errorf("model %q requests must be >= 0", name)
			}
			v := *m.Requests
			mc.Requests = &v
		}
		for k, v := range m.Keys {
			if v < 0 {
				return nil, fmt.Errorf("model %q key %q limit must be >= 0", name, k)
			}
			mc.Keys[k] = v
		}
		cfg.Models[normName] = mc
		cfg.modelOrder = append(cfg.modelOrder, normName)
	}

	return cfg, nil
}

func (c *Config) Enabled() bool {
	if c == nil {
		return false
	}
	if c.Requests > 0 && c.Window > 0 {
		return true
	}
	return len(c.Models) > 0
}
