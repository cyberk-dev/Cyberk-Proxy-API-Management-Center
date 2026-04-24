package weightedselector

import (
	"sort"
	"testing"
)

func TestSWRRClassicNginxSequence(t *testing.T) {
	// Classic nginx SWRR example: weights a=5, b=1, c=1 -> sequence a,a,b,a,c,a,a over 7 steps.
	p := &pool{}
	ids := []string{"a", "b", "c"}
	weights := map[string]int{"a": 5, "b": 1, "c": 1}

	want := []string{"a", "a", "b", "a", "c", "a", "a"}
	for i, expected := range want {
		got := p.pick(ids, weights)
		if got != expected {
			t.Fatalf("step %d: got %q, want %q (sequence so far mismatched)", i, got, expected)
		}
	}
}

func TestSWRRProPlusRatio(t *testing.T) {
	// pro=10, plus=1 -> over 11 ticks, pro 10x, plus 1x.
	p := &pool{}
	ids := []string{"plus", "pro"}
	weights := map[string]int{"pro": 10, "plus": 1}

	counts := map[string]int{}
	for i := 0; i < 11; i++ {
		id := p.pick(ids, weights)
		counts[id]++
	}
	if counts["pro"] != 10 || counts["plus"] != 1 {
		t.Fatalf("pro=%d plus=%d, want pro=10 plus=1", counts["pro"], counts["plus"])
	}
}

func TestSWRRSingleAuth(t *testing.T) {
	p := &pool{}
	ids := []string{"solo"}
	weights := map[string]int{"solo": 3}
	for i := 0; i < 5; i++ {
		if got := p.pick(ids, weights); got != "solo" {
			t.Fatalf("step %d: got %q, want solo", i, got)
		}
	}
}

func TestSWRRAllZeroWeights(t *testing.T) {
	p := &pool{}
	ids := []string{"a", "b"}
	weights := map[string]int{"a": 0, "b": 0}
	if got := p.pick(ids, weights); got != "" {
		t.Fatalf("got %q, want empty string when all weights are zero", got)
	}
}

func TestSWRREmptyInput(t *testing.T) {
	p := &pool{}
	if got := p.pick(nil, nil); got != "" {
		t.Fatalf("got %q on nil input, want empty", got)
	}
	if got := p.pick([]string{}, map[string]int{}); got != "" {
		t.Fatalf("got %q on empty input, want empty", got)
	}
}

func TestSWRRAuthSetChangeRebuilds(t *testing.T) {
	// Start with a,b; switch to a,c mid-sequence. No panic, no "b" appearing after switch.
	p := &pool{}
	firstIDs := []string{"a", "b"}
	w1 := map[string]int{"a": 1, "b": 1}
	for i := 0; i < 4; i++ {
		_ = p.pick(firstIDs, w1)
	}

	secondIDs := []string{"a", "c"}
	w2 := map[string]int{"a": 1, "c": 3}
	seen := map[string]int{}
	for i := 0; i < 8; i++ {
		id := p.pick(secondIDs, w2)
		if id != "a" && id != "c" {
			t.Fatalf("step %d: got %q, want a or c", i, id)
		}
		seen[id]++
	}
	// 1:3 ratio over 8 ticks -> c=6, a=2.
	if seen["c"] != 6 || seen["a"] != 2 {
		t.Fatalf("after switch: a=%d c=%d, want a=2 c=6", seen["a"], seen["c"])
	}
}

func TestSWRRCooldownSkipRedistributes(t *testing.T) {
	// pro has weight 10, plus weight 1. If pro is filtered out (cooldown), all
	// picks should go to plus even though plus has low weight, because the
	// caller passes only eligible IDs.
	p := &pool{}
	full := []string{"plus", "pro"}
	only := []string{"plus"}
	weights := map[string]int{"pro": 10, "plus": 1}

	for i := 0; i < 5; i++ {
		p.pick(full, weights)
	}
	for i := 0; i < 5; i++ {
		if got := p.pick(only, weights); got != "plus" {
			t.Fatalf("step %d after pro cooldown: got %q, want plus", i, got)
		}
	}
	// And once pro is back, distribution resumes (over 11 fresh ticks).
	counts := map[string]int{}
	for i := 0; i < 11; i++ {
		counts[p.pick(full, weights)]++
	}
	if counts["pro"] != 10 || counts["plus"] != 1 {
		t.Fatalf("post-recovery: pro=%d plus=%d, want pro=10 plus=1", counts["pro"], counts["plus"])
	}
}

func TestSWRRWeightChangeTakesEffect(t *testing.T) {
	// Same auth IDs, but operator bumps pro weight; pool must rebuild.
	p := &pool{}
	ids := []string{"plus", "pro"}
	w1 := map[string]int{"plus": 1, "pro": 1}
	for i := 0; i < 4; i++ {
		p.pick(ids, w1)
	}

	w2 := map[string]int{"plus": 1, "pro": 4}
	counts := map[string]int{}
	for i := 0; i < 5; i++ {
		counts[p.pick(ids, w2)]++
	}
	if counts["pro"] != 4 || counts["plus"] != 1 {
		t.Fatalf("after weight change: pro=%d plus=%d, want pro=4 plus=1", counts["pro"], counts["plus"])
	}
}

func TestSWRRDistributionSanity(t *testing.T) {
	// Larger sanity check: 3 auths, non-trivial weights, verify exact counts.
	p := &pool{}
	ids := []string{"x", "y", "z"}
	weights := map[string]int{"x": 3, "y": 2, "z": 1}

	const iters = 600
	counts := map[string]int{}
	for i := 0; i < iters; i++ {
		counts[p.pick(ids, weights)]++
	}
	// 3:2:1 ratio over 600 -> 300 / 200 / 100 exactly.
	if counts["x"] != 300 || counts["y"] != 200 || counts["z"] != 100 {
		t.Fatalf("x=%d y=%d z=%d, want 300/200/100", counts["x"], counts["y"], counts["z"])
	}

	// Check all IDs appeared in sorted order at least once.
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) != 3 {
		t.Fatalf("got %d distinct ids, want 3", len(keys))
	}
}
