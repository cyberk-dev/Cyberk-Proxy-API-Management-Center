package promptlog

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// templatesFileName is the sidecar JSONL co-located with prompts-*.jsonl.
// Append-only: new templates are added at end-of-file; updates to occurrence
// counts / last_seen are persisted by rewriting the whole file periodically
// (see TemplateStore.flush). Format: one Template per line.
const templatesFileName = "templates.jsonl"

// templateHashLen is the truncated SHA-256 hex prefix used to identify a
// template. 12 hex chars = 48 bits of collision space — plenty for the
// ≤ 10⁴ template universe we expect, while staying short enough to fit
// inline in the JSONL `prompt_template` field without bloating each entry.
const templateHashLen = 12

// Template is a registered prefix-template: every prompt starting with Text
// has its prefix replaced by Hash before being written to the JSONL log.
// Source records who created it ("preloaded" for built-in seeds, "detector"
// for ones the dynamic detector found, "manual" for hand-curated ones).
type Template struct {
	Hash        string    `json:"hash"`
	Length      int       `json:"len"`
	Source      string    `json:"source,omitempty"`
	Label       string    `json:"label,omitempty"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	Occurrences int       `json:"occurrences"`
	Text        string    `json:"text"`
}

// TemplateStore is the file-backed catalog of registered templates. Lookups
// (Match) are lock-protected with an RWMutex — common case is many concurrent
// readers (writer middleware) and rare writers (detector + Touch).
//
// Match uses a rune-trie keyed on the first templateMatchRunes runes of each
// template, then verifies the full text via byte-comparison; this keeps the
// hot path O(prefix_runes) without materializing every template per Match.
type TemplateStore struct {
	path string

	mu      sync.RWMutex
	byHash  map[string]*Template
	root    *trieNode  // trie keyed on rune-prefix of template.Text
	dirty   bool       // true when stats changed since last flush
}

// trieNode keys on rune (handles UTF-8 prompts cleanly without byte-cut
// ambiguities) and stores at each terminal node every template whose Text
// equals the path-from-root. Most nodes terminate exactly one template, but
// nesting (one template a strict prefix of another) is handled by storing a
// list and picking the longest match in Match.
type trieNode struct {
	children map[rune]*trieNode
	templates []*Template // templates that terminate exactly at this node
}

// NewTemplateStore loads templates.jsonl from dir if it exists, returning an
// empty (but writable) store when the file is missing. Malformed lines are
// logged and skipped — never abort startup over one bad row.
func NewTemplateStore(dir string) (*TemplateStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("promptlog: templates: empty dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("promptlog: templates: mkdir %s: %w", dir, err)
	}
	s := &TemplateStore{
		path:   filepath.Join(dir, templatesFileName),
		byHash: map[string]*Template{},
		root:   &trieNode{children: map[rune]*trieNode{}},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *TemplateStore) load() error {
	f, err := os.Open(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("promptlog: templates: open: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var t Template
		if err := json.Unmarshal(line, &t); err != nil {
			log.Warnf("promptlog: templates: skip malformed: %v", err)
			continue
		}
		if t.Hash == "" || t.Text == "" {
			continue
		}
		s.byHash[t.Hash] = &t
		s.insertTrie(&t)
	}
	return sc.Err()
}

func (s *TemplateStore) insertTrie(t *Template) {
	n := s.root
	for _, r := range t.Text {
		c, ok := n.children[r]
		if !ok {
			c = &trieNode{children: map[rune]*trieNode{}}
			n.children[r] = c
		}
		n = c
	}
	n.templates = append(n.templates, t)
}

// Match returns the longest registered template whose Text is a prefix of
// prompt, plus the suffix that follows. The bool is false when no template
// matches; callers should fall back to logging the full prompt.
//
// Per-call cost is O(min(len(prompt), longest_template_len)) — a single trie
// walk that records the deepest terminal seen.
func (s *TemplateStore) Match(prompt string) (hash, suffix string, ok bool) {
	if prompt == "" {
		return "", "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.byHash) == 0 {
		return "", "", false
	}
	n := s.root
	var bestTpl *Template
	bestSuffixStart := 0
	consumed := 0
	for i, r := range prompt {
		c, found := n.children[r]
		if !found {
			break
		}
		n = c
		consumed = i + len(string(r))
		if len(n.templates) > 0 {
			// Prefer the longest template terminating here.
			for _, t := range n.templates {
				if bestTpl == nil || t.Length > bestTpl.Length {
					bestTpl = t
					bestSuffixStart = consumed
				}
			}
		}
	}
	if bestTpl == nil {
		return "", "", false
	}
	return bestTpl.Hash, prompt[bestSuffixStart:], true
}

// Register adds a new template (or returns the existing hash if Text is
// already known). Source labels who registered it for later auditing. The
// returned Template is the canonical copy held in-store; do not mutate.
func (s *TemplateStore) Register(text, source string, now time.Time) (*Template, error) {
	if len(text) == 0 {
		return nil, fmt.Errorf("promptlog: templates: empty text")
	}
	hash := hashTemplate(text)
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.byHash[hash]; ok {
		return existing, nil
	}
	t := &Template{
		Hash:        hash,
		Length:      len(text),
		Source:      source,
		FirstSeen:   now,
		LastSeen:    now,
		Occurrences: 0,
		Text:        text,
	}
	s.byHash[hash] = t
	s.insertTrie(t)
	if err := s.appendLine(t); err != nil {
		// Roll back the in-memory side so we don't lie about durability.
		delete(s.byHash, hash)
		return nil, err
	}
	return t, nil
}

// Touch increments occurrence count and bumps LastSeen for an existing
// template. No-op (silently) when the hash is unknown — the writer should
// not crash if a template was concurrently deleted.
func (s *TemplateStore) Touch(hash string, when time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byHash[hash]
	if !ok {
		return
	}
	t.Occurrences++
	if when.After(t.LastSeen) {
		t.LastSeen = when
	}
	s.dirty = true
}

// Get returns a deep copy of the template (so callers cannot mutate the
// in-store record). Returns false when hash is unknown.
func (s *TemplateStore) Get(hash string) (Template, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.byHash[hash]
	if !ok {
		return Template{}, false
	}
	return *t, true
}

// List returns deep-copied templates sorted by LastSeen desc (most recently
// seen first), then by Hash for stable ordering. Cheap clone — text is
// shared via Go's string interning, only the struct fields are copied.
func (s *TemplateStore) List() []Template {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Template, 0, len(s.byHash))
	for _, t := range s.byHash {
		out = append(out, *t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].Hash < out[j].Hash
	})
	return out
}

// Flush rewrites templates.jsonl with current state — used to persist
// occurrence/last-seen drift. Atomic via tmp + rename.
func (s *TemplateStore) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	if err := s.rewriteLocked(); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// appendLine appends a single template to disk. Called inside Register with
// the lock held.
func (s *TemplateStore) appendLine(t *Template) error {
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("promptlog: templates: append: %w", err)
	}
	defer f.Close()
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := f.Write(b); err != nil {
		return err
	}
	return f.Sync()
}

func (s *TemplateStore) rewriteLocked() error {
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(f)
	for _, t := range s.byHash {
		b, err := json.Marshal(t)
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if _, err := bw.Write(b); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if err := bw.WriteByte('\n'); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, s.path)
}

// hashTemplate returns the canonical hash key for a template body. Truncated
// to templateHashLen so the value stays compact in JSONL entries.
func hashTemplate(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:templateHashLen]
}
