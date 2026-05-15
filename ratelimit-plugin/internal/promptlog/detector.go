package promptlog

import (
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// templateDetector watches the stream of prompts flowing through Writer.run
// and registers prefixes that recur frequently enough to qualify as
// templates. Detection runs entirely in-process on a rolling window of
// recent prompts so it never opens or scans the JSONL log.
//
// The data structure is a rune-keyed trie where each path-from-root tracks a
// "leaves" counter — the number of distinct observed prompts whose first N
// runes follow that path. After observing the configured number of prompts
// (or after the scan tick), the detector walks the trie and registers, for
// each branch, the deepest node whose leaves ≥ MinOccur and depth ≥ MinLen.
//
// The trie is bounded by Window: only the last Window prompts contribute,
// and a ring of recent prompt entries lets us decrement-and-prune as new
// observations evict old ones. Memory cost is O(window * avg_runes_kept).
type templateDetector struct {
	store *TemplateStore
	cfg   TemplatesConfig

	mu     sync.Mutex
	root   *detectorNode
	window []detectorObs
	head   int // ring write index
	full   bool

	scanEvery time.Duration
	lastScan  time.Time
	sinceScan int

	// maxRunesObserved caps how deep into a prompt the trie tracks. Beyond
	// this no template can be detected — a small multiple of MinLen is
	// enough for prefix-template work.
	maxRunesObserved int
}

type detectorNode struct {
	leaves   int
	children map[rune]*detectorNode
}

type detectorObs struct {
	runes []rune // capped to maxRunesObserved
}

func newTemplateDetector(store *TemplateStore, cfg TemplatesConfig) *templateDetector {
	if cfg.MinLen <= 0 {
		cfg.MinLen = defaultTplMinLen
	}
	if cfg.MinOccur <= 0 {
		cfg.MinOccur = defaultTplMinOccur
	}
	if cfg.Window <= 0 {
		cfg.Window = defaultTplWindow
	}
	if cfg.ScanEvery <= 0 {
		cfg.ScanEvery = defaultTplScanEvery
	}
	return &templateDetector{
		store:            store,
		cfg:              cfg,
		root:             &detectorNode{children: map[rune]*detectorNode{}},
		window:           make([]detectorObs, cfg.Window),
		scanEvery:        time.Duration(cfg.ScanEvery) * time.Second,
		maxRunesObserved: cfg.MinLen * 4, // depth budget — wide enough for the longest realistic template
	}
}

// observe records prompt against the rolling window and triggers a scan
// when either the time tick or the per-N-write fallback fires. Called on
// the writer goroutine — synchronizes via its own mutex so future expansion
// to a separate scanner goroutine stays safe.
func (d *templateDetector) observe(prompt string, now time.Time) {
	if d == nil || prompt == "" {
		return
	}
	runes := []rune(prompt)
	if len(runes) > d.maxRunesObserved {
		runes = runes[:d.maxRunesObserved]
	}
	if len(runes) < d.cfg.MinLen {
		// Too short to ever produce a template; skip without consuming a
		// window slot to keep the window focused on candidate prompts.
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.full {
		// Evict the slot we are about to overwrite.
		d.evictLocked(d.window[d.head].runes)
	}
	d.window[d.head].runes = runes
	d.insertLocked(runes)
	d.head = (d.head + 1) % d.cfg.Window
	if d.head == 0 {
		d.full = true
	}

	d.sinceScan++
	if d.lastScan.IsZero() {
		d.lastScan = now
	}
	due := d.sinceScan >= d.cfg.Window/5
	if !due && d.scanEvery > 0 && now.Sub(d.lastScan) >= d.scanEvery {
		due = true
	}
	if due {
		d.scanLocked(now)
		d.lastScan = now
		d.sinceScan = 0
	}
}

func (d *templateDetector) insertLocked(runes []rune) {
	n := d.root
	for _, r := range runes {
		c, ok := n.children[r]
		if !ok {
			c = &detectorNode{children: map[rune]*detectorNode{}}
			n.children[r] = c
		}
		c.leaves++
		n = c
	}
}

func (d *templateDetector) evictLocked(runes []rune) {
	n := d.root
	for _, r := range runes {
		c, ok := n.children[r]
		if !ok {
			return
		}
		c.leaves--
		if c.leaves <= 0 {
			delete(n.children, r)
			return
		}
		n = c
	}
}

// scanLocked walks the trie depth-first; for each branch records the
// deepest node satisfying the registration thresholds and registers the
// path-from-root as a template. Must hold d.mu.
func (d *templateDetector) scanLocked(now time.Time) {
	if d.store == nil {
		return
	}
	var path []rune
	d.visit(d.root, &path, now)
}

func (d *templateDetector) visit(n *detectorNode, path *[]rune, now time.Time) {
	// Find the deepest descendant that still meets the min-occurrence
	// threshold along a single chain. The "deepest qualifying ancestor"
	// approach yields the longest stable common prefix without registering
	// every shorter ancestor too.
	deepest := -1
	d.recurseDeepest(n, path, len(*path), &deepest)
	if deepest >= d.cfg.MinLen && deepest <= len(*path) {
		text := string((*path)[:deepest])
		// Skip if the prompt already starts with a longer registered template.
		if hash, _, ok := d.store.Match(text); ok {
			if existing, found := d.store.Get(hash); found && existing.Length >= deepest {
				return
			}
		}
		if _, err := d.store.Register(text, "detector", now); err != nil {
			log.Warnf("promptlog: detector: register: %v", err)
		}
	}
	// Recurse into children that themselves carry enough leaves to host
	// independent templates (different first-rune branches register
	// separately).
	for r, c := range n.children {
		if c.leaves < d.cfg.MinOccur {
			continue
		}
		*path = append(*path, r)
		d.visit(c, path, now)
		*path = (*path)[:len(*path)-1]
	}
}

// recurseDeepest follows a single chain (only when the next child carries
// the same leaf count → still a "common" prefix) and reports the deepest
// length where that chain still meets MinOccur.
func (d *templateDetector) recurseDeepest(n *detectorNode, path *[]rune, depth int, deepest *int) {
	if n.leaves < d.cfg.MinOccur && depth > 0 {
		return
	}
	if depth > *deepest {
		*deepest = depth
	}
	// Only descend when there is a uniquely-shared child carrying the
	// SAME leaf count; otherwise the chain branches and the shared prefix
	// ends here.
	if len(n.children) != 1 {
		return
	}
	for r, c := range n.children {
		if c.leaves != n.leaves && depth > 0 {
			return
		}
		*path = append(*path, r)
		d.recurseDeepest(c, path, depth+1, deepest)
		*path = (*path)[:len(*path)-1]
	}
}
