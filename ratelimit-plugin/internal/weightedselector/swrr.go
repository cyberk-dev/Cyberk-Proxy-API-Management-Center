package weightedselector

import (
	"sort"
	"sync"
)

// pool holds smooth-weighted-round-robin state for a single (provider, model) key.
//
// The algorithm is the classic nginx smooth weighted round-robin: for every call
// each node's currentWeight is incremented by its weight, the node with the
// highest currentWeight is chosen, and its currentWeight is reduced by the total
// weight. Over time this produces a deterministic interleaved sequence whose
// frequency per node matches its weight share.
type pool struct {
	mu      sync.Mutex
	authIDs []string
	nodes   []*node
}

type node struct {
	authID        string
	weight        int
	currentWeight int
}

// pick runs one SWRR step. The caller has already filtered out cooldown/priority
// losers; the input slice contains only eligible auths for this call.
// weights maps authID -> effective weight (derived from plan_type via Config).
// Returns the chosen authID, or "" if every eligible auth has weight <= 0.
func (p *pool) pick(authIDs []string, weights map[string]int) string {
	if len(authIDs) == 0 {
		return ""
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.syncNodes(authIDs, weights)
	if len(p.nodes) == 0 {
		return ""
	}

	total := 0
	var best *node
	for _, n := range p.nodes {
		if n.weight <= 0 {
			continue
		}
		total += n.weight
		n.currentWeight += n.weight
		if best == nil || n.currentWeight > best.currentWeight {
			best = n
		}
	}
	if best == nil || total == 0 {
		return ""
	}
	best.currentWeight -= total
	return best.authID
}

// syncNodes rebuilds the node list whenever the eligible auth set changes
// between calls (e.g., a cooldown kicked in, or a new auth was loaded). It
// preserves currentWeight for auths that survive across the rebuild so SWRR
// doesn't restart its interleaving on every transient membership shift.
// Must be called with p.mu held.
func (p *pool) syncNodes(authIDs []string, weights map[string]int) {
	sorted := append([]string(nil), authIDs...)
	sort.Strings(sorted)

	changed := len(sorted) != len(p.authIDs)
	if !changed {
		for i := range sorted {
			if sorted[i] != p.authIDs[i] {
				changed = true
				break
			}
		}
	}

	// Weight may change even if the auth set didn't (e.g. operator edited YAML
	// and hot-reloaded). Detect drift so the fresh weight wins.
	if !changed {
		for _, n := range p.nodes {
			if weights[n.authID] != n.weight {
				changed = true
				break
			}
		}
	}

	if !changed {
		return
	}

	prev := make(map[string]int, len(p.nodes))
	for _, n := range p.nodes {
		prev[n.authID] = n.currentWeight
	}

	p.authIDs = sorted
	p.nodes = p.nodes[:0]
	for _, id := range sorted {
		w := weights[id]
		if w < 0 {
			w = 0
		}
		p.nodes = append(p.nodes, &node{
			authID:        id,
			weight:        w,
			currentWeight: prev[id],
		})
	}
}
