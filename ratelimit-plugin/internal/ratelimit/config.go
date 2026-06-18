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

	modelOrder   []string
	resolveCache sync.Map

	// aliasCanonical maps a normalized OAuth model alias to its upstream model
	// name (e.g. "claude-opus-4-8" -> "gpt-5.5"). Built from the top-level
	// `oauth-model-alias` config. See buildAliasCanonical and Canonical.
	aliasCanonical map[string]string
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

// Canonical maps an OAuth model alias to its upstream model name so that all
// aliases forking to the same upstream share one rate-limit counter and cap.
// The proxy core resolves aliases to their upstream model *after* the
// rate-limit middleware runs, so without this an alias like "claude-opus-4-8"
// (which forks to gpt-5.5) would get its own counter and bypass the gpt-5.5
// per-key cap.
//
// The input is first stripped of any trailing thinking suffix "(value)"
// (mirroring the core's thinking.ParseSuffix, which the core also applies
// before alias resolution) so that "gpt-5.5(high)" and "claude-opus-4-8(high)"
// collapse onto the same counter as the bare upstream rather than fragmenting
// into per-suffix counters that dodge the cap.
//
// Only aliases that map to a single upstream across all providers are
// canonicalized; ambiguous aliases (the serving provider is unknown at this
// layer) are returned normalized but otherwise unchanged, so they fall through
// to wildcard/default limits as before. Alias→upstream resolution is single-hop
// by design: it mirrors the core, which resolves one alias level per provider.
// The return value is always normalized (lowercased/trimmed, suffix stripped)
// to match how resolveUncached and extract.go treat model names.
func (c *Config) Canonical(model string) string {
	norm := stripThinkingSuffix(strings.ToLower(strings.TrimSpace(model)))
	if c == nil || c.aliasCanonical == nil {
		return norm
	}
	if upstream, ok := c.aliasCanonical[norm]; ok {
		return upstream
	}
	return norm
}

// stripThinkingSuffix removes a trailing "(value)" thinking suffix from a model
// name, mirroring the upstream SDK's thinking.ParseSuffix (format
// "model-name(value)"): split at the LAST "(", but only when the string ends
// with ")". The core strips this suffix before deriving the base model for
// alias resolution, so the limiter must strip it too or suffixed requests key
// on a separate counter and bypass the upstream's cap.
func stripThinkingSuffix(model string) string {
	if !strings.HasSuffix(model, ")") {
		return model
	}
	i := strings.LastIndex(model, "(")
	if i < 0 {
		return model
	}
	return strings.TrimSpace(model[:i])
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
	Ratelimit       rawConfig                  `yaml:"ratelimit"`
	OAuthModelAlias map[string][]rawAliasEntry `yaml:"oauth-model-alias"`
}

// rawAliasEntry mirrors one entry of the top-level `oauth-model-alias.<provider>`
// list: `name` is the upstream model, `alias` is the name clients call it by.
type rawAliasEntry struct {
	Name  string `yaml:"name"`
	Alias string `yaml:"alias"`
	Fork  bool   `yaml:"fork"`
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
	cfg, err := buildConfig(root.Ratelimit)
	if err != nil {
		return nil, err
	}
	cfg.aliasCanonical = buildAliasCanonical(root.OAuthModelAlias)
	return cfg, nil
}

// buildAliasCanonical builds the reverse alias→upstream map consumed by
// Config.Canonical. raw is keyed by provider; each value lists {name, alias}
// pairs where `name` is the upstream model and `alias` is the client-facing
// name.
//
// An alias is canonicalized only when it resolves to exactly one upstream
// across every provider. When the same alias forks to different upstreams under
// different providers (e.g. "claude-sonnet-4-6" → gpt-5.4 under codex but a
// claude model under kiro), the serving provider is not known at the
// rate-limit layer, so the alias is dropped from the map and left to match
// wildcard/default limits unchanged.
func buildAliasCanonical(raw map[string][]rawAliasEntry) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	// alias -> set of distinct upstream names seen across all providers.
	upstreams := map[string]map[string]struct{}{}
	for _, entries := range raw {
		for _, e := range entries {
			alias := strings.ToLower(strings.TrimSpace(e.Alias))
			name := strings.ToLower(strings.TrimSpace(e.Name))
			if alias == "" || name == "" || alias == name {
				continue
			}
			set := upstreams[alias]
			if set == nil {
				set = map[string]struct{}{}
				upstreams[alias] = set
			}
			set[name] = struct{}{}
		}
	}
	out := make(map[string]string, len(upstreams))
	for alias, set := range upstreams {
		if len(set) != 1 {
			continue // ambiguous across providers — leave uncanonicalized
		}
		for name := range set {
			out[alias] = name
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
