package promptlog

import (
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// Index is an in-memory derived view of the JSONL files on disk. JSONL files
// remain the source of truth — the index is rebuilt from scratch on every
// process start (see NewIndex) and mutated by the Writer goroutine via Add
// whenever a new entry lands on disk.
//
// Concurrency: a single Writer goroutine calls Add (W lock); gin handlers
// query through ListUsers / BuildDetail / SearchMessages (R lock). Since
// there is exactly one writer, the W lock isn't strictly needed for
// correctness on the Add path, but holding it makes the reader/writer
// guarantee explicit and lets future code add a second writer (e.g. a
// retention sweeper) without revisiting invariants.
type Index struct {
	mu     sync.RWMutex
	byHash map[string]*keyShard
}

// keyShard holds every retained Entry for one key_hash, in arrival order.
// Blocks are stripped before the entry lands here (the writer does this at
// writer.go:270–278; cold-start strips again defensively).
//
// TODO: when a single key exceeds ~100k entries, the append-doubling realloc
// becomes painful (a 100k-entry []Entry move is ~5 MB). Switch to a chunked
// slice-of-slices (e.g. 4k entries per chunk) at that point. The query
// methods walk entries sequentially, so chunking changes only the inner
// iteration shape.
type keyShard struct {
	entries []Entry
}

// NewIndex builds a fresh Index by replaying every prompts-*.jsonl file in
// dir. Cost is O(total entries on disk) at boot; subsequent reads are RAM-
// only. Returns an empty (but usable) index when dir is empty or missing —
// matches the behavior of the underlying scanAll.
//
// File scanning is parallel: one goroutine per file (capped to GOMAXPROCS)
// reuses the existing scanFile helper so the 16 MiB per-line token cap from
// reader.go is preserved. Merge into byHash is single-threaded AND walks
// files in sorted filename order, so parent-session last-writer-wins
// semantics (reader.go bucketFor) remain deterministic across reboots —
// channel-arrival order would be nondeterministic.
func NewIndex(dir string) (*Index, error) {
	idx := &Index{byHash: make(map[string]*keyShard)}
	if dir == "" {
		return idx, nil
	}
	files, err := filepath.Glob(filepath.Join(dir, "prompts-*.jsonl"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return idx, nil
	}
	sort.Strings(files)

	start := time.Now()
	results, perKeyCount := scanFilesParallel(files)

	// Pre-size shards: a single allocation per key keeps cold-start out of
	// the append-doubling realloc path. Pass 1 (scanFilesParallel) already
	// counted entries per key.
	for hash, n := range perKeyCount {
		idx.byHash[hash] = &keyShard{entries: make([]Entry, 0, n)}
	}

	totalEntries := 0
	for _, f := range files {
		for _, e := range results[f] {
			// Defensive strip — the writer already drops Blocks before
			// they hit disk (writer.go:270–278), but a legacy file or
			// hand-edited fixture might still carry them.
			e.Blocks = nil
			// perKeyCount is built from the same results map, so every
			// key we see here has a pre-sized shard. No nil check needed.
			idx.byHash[e.KeyHash].entries = append(idx.byHash[e.KeyHash].entries, e)
			totalEntries++
		}
	}

	log.Infof("promptlog: index boot dir=%s files=%d entries=%d keys=%d dur=%s",
		dir, len(files), totalEntries, len(idx.byHash), time.Since(start))
	return idx, nil
}

// scanFilesParallel reads every file in parallel and returns one []Entry per
// file plus a key-count tally used for shard pre-sizing. Workers reuse the
// existing scanFile helper so the 16 MiB per-line cap is preserved.
//
// Parse errors per line are already swallowed inside scanFile (logged at
// debug); a partial scan still produces a usable index — consistent with
// scanAll's behavior at L48–52 of reader.go.
func scanFilesParallel(files []string) (map[string][]Entry, map[string]int) {
	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	if workers > len(files) {
		workers = len(files)
	}

	type job struct {
		path    string
		entries []Entry
	}
	jobs := make(chan int, len(files))
	out := make([]job, len(files))
	for i := range files {
		jobs <- i
	}
	close(jobs)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				local := make([]Entry, 0, 64)
				scanFile(files[i], func(e Entry) bool {
					local = append(local, e)
					return true
				})
				// Workers write to DISTINCT indices of out[] — no shared
				// slot, so no mutex needed. Do NOT "fix" this by adding
				// locking; you'd serialize the parallel scan.
				out[i] = job{path: files[i], entries: local}
			}
		}()
	}
	wg.Wait()

	results := make(map[string][]Entry, len(files))
	perKey := make(map[string]int)
	for _, j := range out {
		results[j.path] = j.entries
		for _, e := range j.entries {
			perKey[e.KeyHash]++
		}
	}
	return results, perKey
}

// Add appends a single Entry to its key shard. Called from the Writer
// goroutine after the entry has been written to the JSONL buffer (writer
// run loop). Blocks are stripped beforehand at writer.go:270–278; we do
// NOT defensively re-strip here because Add is the hot path — the cost of
// a redundant per-block walk on every captured prompt isn't worth the
// belt-and-braces.
//
// A nil receiver is a no-op so the writer can run without an index in
// tests / when prompts are disabled.
func (i *Index) Add(e *Entry) {
	if i == nil || e == nil {
		return
	}
	i.mu.Lock()
	shard := i.byHash[e.KeyHash]
	if shard == nil {
		shard = &keyShard{}
		i.byHash[e.KeyHash] = shard
	}
	shard.entries = append(shard.entries, *e)
	i.mu.Unlock()
}

// ListUsers returns one row per known key (active + configured-but-empty),
// matching the shape of the scan-based top-level ListUsers. Pure in-memory;
// no error path.
//
// Under the RLock we snapshot just the slice headers — writers only append,
// so readers iterating the captured len are safe to release the lock and
// continue aggregating without locking out further Adds.
func (i *Index) ListUsers(configuredKeys []string) []UserSummary {
	if i == nil {
		return aggregateUsers(nil, configuredKeys)
	}
	i.mu.RLock()
	perKey := make(map[string][]Entry, len(i.byHash))
	for hash, shard := range i.byHash {
		perKey[hash] = shard.entries
	}
	i.mu.RUnlock()
	return aggregateUsers(perKey, configuredKeys)
}

// BuildDetail returns the per-key tree for keyHash with the same opts
// semantics as the scan-based BuildDetail. Returns a zero-valued Detail
// (with the keyHash/hint/configured set) when the index has no entries
// for the key — matches the behavior of a scan over an empty store.
func (i *Index) BuildDetail(keyHash, configuredHint string, configured bool, opts DetailOpts) *Detail {
	if i == nil {
		return aggregateDetail(nil, keyHash, configuredHint, configured, opts)
	}
	i.mu.RLock()
	var entries []Entry
	if shard, ok := i.byHash[keyHash]; ok {
		// Capture slice header; safe because Add only appends and we never
		// mutate existing indices.
		entries = shard.entries
	}
	i.mu.RUnlock()
	return aggregateDetail(entries, keyHash, configuredHint, configured, opts)
}

// SearchMessages runs the same case-insensitive prompt search as the
// scan-based SearchMessages, but on already-parsed in-memory entries.
func (i *Index) SearchMessages(keyHash, query string, limit int) *SearchResult {
	if i == nil {
		return aggregateSearch(nil, query, limit)
	}
	i.mu.RLock()
	var entries []Entry
	if shard, ok := i.byHash[keyHash]; ok {
		entries = shard.entries
	}
	i.mu.RUnlock()
	return aggregateSearch(entries, query, limit)
}
