package weightedselector

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config controls weighted routing for Codex accounts. Non-codex providers are
// unaffected regardless of this config.
type Config struct {
	// Enabled is true iff the operator supplied a codex_weights block in YAML.
	// When false, the caller skips injecting a custom coreauth.Manager and the
	// SDK uses its default routing (round-robin or fill-first).
	Enabled bool

	// Weights maps a normalized plan_type (lowercase, stripped of "-"/"_") to a
	// relative weight. Unknown plan_types fall back to defaultFallbackWeight.
	Weights map[string]int
}

// defaultWeights reflects rough quota ratios observed across ChatGPT / Codex
// plans. They are only applied when the operator declares codex_weights but
// omits a specific plan. Missing plans at resolve time (unknown plan_type)
// fall back to defaultFallbackWeight.
var defaultWeights = map[string]int{
	"pro":      10,
	"prolite":  5,
	"plus":     1,
	"free":     1,
	"team":     1,
	"business": 1,
	"go":       1,
}

const defaultFallbackWeight = 1

type rawWeightsRoot struct {
	CodexWeights map[string]int `yaml:"codex_weights"`
}

// LoadFromYAML parses codex_weights from the same config.yaml the plugin already
// loads for ratelimit rules. Absent block -> Enabled=false, no error. Partial
// block -> the operator's entries override defaults for listed plans; unlisted
// plans keep their default weight (so "codex_weights: { pro: 20 }" still gives
// plus=1, free=1, etc.).
func LoadFromYAML(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return parseBytes(data)
}

func parseBytes(data []byte) (Config, error) {
	var root rawWeightsRoot
	if err := yaml.Unmarshal(data, &root); err != nil {
		return Config{}, fmt.Errorf("unmarshal yaml: %w", err)
	}
	if root.CodexWeights == nil {
		return Config{Enabled: false}, nil
	}

	cfg := Config{
		Enabled: true,
		Weights: make(map[string]int, len(defaultWeights)+len(root.CodexWeights)),
	}
	for k, v := range defaultWeights {
		cfg.Weights[k] = v
	}
	for k, v := range root.CodexWeights {
		if v < 0 {
			return Config{}, fmt.Errorf("codex_weights[%q] must be >= 0, got %d", k, v)
		}
		cfg.Weights[normalizePlan(k)] = v
	}
	return cfg, nil
}

// WeightFor returns the configured weight for a plan_type value. Normalization
// matches what the Codex JWT synthesizer writes at auth.Attributes["plan_type"]
// (lowercase, strip "-" / "_") so "pro-lite" == "pro_lite" == "prolite".
// Unknown plans return defaultFallbackWeight (1) so a new ChatGPT tier rolling
// out won't silently route as weight-zero.
func (c Config) WeightFor(planType string) int {
	if !c.Enabled {
		return 0
	}
	key := normalizePlan(planType)
	if key == "" {
		return defaultFallbackWeight
	}
	if w, ok := c.Weights[key]; ok {
		return w
	}
	return defaultFallbackWeight
}

func normalizePlan(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '-' || r == '_' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
