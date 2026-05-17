package promptlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/cyberk/ratelimit-plugin/internal/ratelimit"
)

// scanCallback is invoked once per JSONL line; returning false stops scanning
// early. Errors parsing individual lines are swallowed (logged at debug) so
// one corrupt entry doesn't abort the whole report.
type scanCallback func(Entry) bool

// scanAll walks every prompts-YYYY-MM-DD.jsonl in dir in chronological order
// (oldest file first, lines within a file in append order). A missing dir is
// not an error — it just means no data has been written yet.
func scanAll(dir string, fn scanCallback) error {
	if dir == "" {
		return nil
	}
	files, err := filepath.Glob(filepath.Join(dir, "prompts-*.jsonl"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		// Distinguish missing dir from empty dir for debug visibility.
		if _, statErr := os.Stat(dir); errors.Is(statErr, fs.ErrNotExist) {
			return nil
		}
	}
	sort.Strings(files)
	for _, path := range files {
		if !scanFile(path, fn) {
			return nil
		}
	}
	return nil
}

func scanFile(path string, fn scanCallback) (cont bool) {
	f, err := os.Open(path)
	if err != nil {
		log.Warnf("promptlog: open %s: %v", path, err)
		return true
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// Single entries can be large when text blocks approach max_text_bytes
	// plus inline metadata; allocate a 16 MiB token cap so the scanner does
	// not truncate. Initial buffer is 64 KiB so small files stay cheap.
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			log.Debugf("promptlog: skip malformed line in %s: %v", path, err)
			continue
		}
		if !fn(e) {
			return false
		}
	}
	if err := sc.Err(); err != nil {
		log.Warnf("promptlog: scan %s: %v", path, err)
	}
	return true
}

// UserSummary is one row of the /users list endpoint. Configured is true
// when key_hash matches a key present in the proxy config; entries with
// Configured=false come from JSONL lines whose key has since been removed or
// rotated out, which is still useful for spotting orphaned activity.
type UserSummary struct {
	KeyHash      string    `json:"key_hash"`
	KeyHint      string    `json:"key_hint,omitempty"`
	Configured   bool      `json:"configured"`
	MessageCount int       `json:"message_count"`
	SessionCount int       `json:"session_count"`
	CWDCount     int       `json:"cwd_count"`
	FirstSeen    time.Time `json:"first_seen,omitempty"`
	LastSeen     time.Time `json:"last_seen,omitempty"`
	Clients      []string  `json:"clients,omitempty"`
	Models       []string  `json:"models,omitempty"`
}

// ListUsers aggregates JSONL contents into per-key summaries and unions in
// configured keys that have no activity yet, so the UI can offer the full
// roster even on a fresh install. Sort order: last-seen descending, with
// configured-but-empty keys at the bottom.
func ListUsers(dir string, configuredKeys []string) ([]UserSummary, error) {
	type agg struct {
		msgCount  int
		sessions  map[string]struct{}
		cwds      map[string]struct{}
		clients   map[string]struct{}
		models    map[string]struct{}
		firstSeen time.Time
		lastSeen  time.Time
	}
	byHash := map[string]*agg{}

	if err := scanAll(dir, func(e Entry) bool {
		a := byHash[e.KeyHash]
		if a == nil {
			a = &agg{
				sessions:  make(map[string]struct{}),
				cwds:      make(map[string]struct{}),
				clients:   make(map[string]struct{}),
				models:    make(map[string]struct{}),
				firstSeen: e.Timestamp,
				lastSeen:  e.Timestamp,
			}
			byHash[e.KeyHash] = a
		}
		a.msgCount++
		if e.SessionID != "" {
			a.sessions[e.SessionID] = struct{}{}
		}
		if e.CWD != "" {
			a.cwds[e.CWD] = struct{}{}
		}
		if e.Client != "" {
			a.clients[e.Client] = struct{}{}
		}
		if e.Model != "" {
			a.models[e.Model] = struct{}{}
		}
		if e.Timestamp.After(a.lastSeen) {
			a.lastSeen = e.Timestamp
		}
		if e.Timestamp.Before(a.firstSeen) {
			a.firstSeen = e.Timestamp
		}
		return true
	}); err != nil {
		return nil, err
	}

	hintByHash := map[string]string{}
	for _, k := range configuredKeys {
		hintByHash[ratelimit.HashKey(k)] = MakeKeyHint(k)
	}

	out := make([]UserSummary, 0, len(byHash)+len(configuredKeys))
	for hash, a := range byHash {
		hint, configured := hintByHash[hash]
		out = append(out, UserSummary{
			KeyHash:      hash,
			KeyHint:      hint,
			Configured:   configured,
			MessageCount: a.msgCount,
			SessionCount: len(a.sessions),
			CWDCount:     len(a.cwds),
			FirstSeen:    a.firstSeen,
			LastSeen:     a.lastSeen,
			Clients:      sortedSetKeys(a.clients),
			Models:       sortedSetKeys(a.models),
		})
	}
	// Configured keys with zero activity still belong in the list — without
	// them the UI would hide brand-new keys and look suspicious on a fresh
	// install.
	for _, k := range configuredKeys {
		hash := ratelimit.HashKey(k)
		if _, ok := byHash[hash]; ok {
			continue
		}
		out = append(out, UserSummary{KeyHash: hash, KeyHint: MakeKeyHint(k), Configured: true})
	}

	sort.SliceStable(out, func(i, j int) bool {
		// Active users (any message) first, by last_seen desc; empty configured
		// keys fall to the bottom in stable order.
		if out[i].MessageCount == 0 && out[j].MessageCount == 0 {
			return out[i].KeyHash < out[j].KeyHash
		}
		if out[i].MessageCount == 0 {
			return false
		}
		if out[j].MessageCount == 0 {
			return true
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out, nil
}

// Detail is the per-user tree returned by /users/:key. Groups are sorted by
// last-seen descending so the cwd the user just worked in surfaces first.
type Detail struct {
	KeyHash       string     `json:"key_hash"`
	KeyHint       string     `json:"key_hint,omitempty"`
	Configured    bool       `json:"configured"`
	TotalMessages int        `json:"total_messages"`
	TotalSessions int        `json:"total_sessions"`
	TotalCWDs     int        `json:"total_cwds"`
	Groups        []CWDGroup `json:"groups"`
}

type CWDGroup struct {
	CWD          string    `json:"cwd"`
	MessageCount int       `json:"message_count"`
	LastSeen     time.Time `json:"last_seen"`
	Sessions     []Session `json:"sessions"`
}

type Session struct {
	SessionID     string    `json:"session_id"`
	Client        string    `json:"client"`
	ClientVersion string    `json:"client_version,omitempty"`
	Models        []string  `json:"models"`
	FirstSeen     time.Time `json:"first_seen"`
	LastSeen      time.Time `json:"last_seen"`
	MessageCount  int       `json:"message_count"`
	Truncated     bool      `json:"truncated,omitempty"`
	Messages      []Message `json:"messages"`
}

// Message is a slim view used in the tree — heavy fields like Blocks are
// dropped here to keep the response small; the UI fetches full detail (with
// blocks) lazily via the detail endpoint with a session filter, if needed.
//
// PromptTemplate, when set, is the hash of a registered template whose body
// is the prefix of the original prompt; Prompt then holds only the suffix.
// The UI reconstructs the full text by fetching /templates/:hash and
// concatenating, OR the caller can pass `?inline_templates=1` to BuildDetail
// so the server splices the template back in.
type Message struct {
	Timestamp      time.Time `json:"ts"`
	Model          string    `json:"model,omitempty"`
	Provider       string    `json:"provider,omitempty"`
	Status         int       `json:"status"`
	Role           string    `json:"role,omitempty"`
	Prompt         string    `json:"prompt"`
	PromptTemplate string    `json:"prompt_template,omitempty"`
	Blocks         []Block   `json:"blocks,omitempty"`
}

// InlineTemplates rewrites every Message in detail by splicing the matching
// template body back into Prompt and clearing PromptTemplate. Used when the
// caller wants the response self-contained (e.g. dashboards that don't want
// to make a second round-trip to /templates/:hash). When templates is nil,
// no-op.
func InlineTemplates(detail *Detail, templates *TemplateStore) {
	if detail == nil || templates == nil {
		return
	}
	for gi := range detail.Groups {
		for si := range detail.Groups[gi].Sessions {
			msgs := detail.Groups[gi].Sessions[si].Messages
			for mi := range msgs {
				h := msgs[mi].PromptTemplate
				if h == "" {
					continue
				}
				if t, ok := templates.Get(h); ok {
					msgs[mi].Prompt = t.Text + msgs[mi].Prompt
					msgs[mi].PromptTemplate = ""
				}
			}
		}
	}
}

// BuildDetail scans the JSONL store filtering by keyHash and returns a
// tree grouped by cwd → session. perSessionLimit caps the messages array
// per session (keeping the most recent N); sessions that exceed the cap
// are marked Truncated for UI display.
func BuildDetail(dir, keyHash string, configuredHint string, configured bool, perSessionLimit int) (*Detail, error) {
	if perSessionLimit <= 0 {
		perSessionLimit = 200
	}
	type sessAgg struct {
		client        string
		clientVersion string
		models        map[string]struct{}
		firstSeen     time.Time
		lastSeen      time.Time
		msgs          []Message
		total         int
	}
	type cwdAgg struct {
		sessions  map[string]*sessAgg
		msgCount  int
		lastSeen  time.Time
	}
	byCWD := map[string]*cwdAgg{}
	totalSessions := map[string]struct{}{}
	totalMessages := 0

	if err := scanAll(dir, func(e Entry) bool {
		if e.KeyHash != keyHash {
			return true
		}
		totalMessages++
		cwd := e.CWD
		if cwd == "" {
			cwd = "(unknown)"
		}
		c := byCWD[cwd]
		if c == nil {
			c = &cwdAgg{sessions: make(map[string]*sessAgg)}
			byCWD[cwd] = c
		}
		c.msgCount++
		if e.Timestamp.After(c.lastSeen) {
			c.lastSeen = e.Timestamp
		}

		sid := e.SessionID
		if sid == "" {
			// Synthesize a per-cwd "no-session" bucket so messages without a
			// session header still appear in the tree.
			sid = "(no-session)"
		}
		totalSessions[cwd+"|"+sid] = struct{}{}

		s := c.sessions[sid]
		if s == nil {
			s = &sessAgg{
				models:    map[string]struct{}{},
				firstSeen: e.Timestamp,
				lastSeen:  e.Timestamp,
			}
			c.sessions[sid] = s
		}
		s.total++
		if s.client == "" {
			s.client = e.Client
			s.clientVersion = e.ClientVersion
		}
		if e.Model != "" {
			s.models[e.Model] = struct{}{}
		}
		if e.Timestamp.After(s.lastSeen) {
			s.lastSeen = e.Timestamp
		}
		if e.Timestamp.Before(s.firstSeen) {
			s.firstSeen = e.Timestamp
		}
		// Keep the most recent perSessionLimit messages via a sliding window.
		role := e.Role
		if role == "" {
			// Legacy entries (written before assistant-side capture existed)
			// have no role field. Normalize to "user" here so downstream
			// consumers — including any strict `role === 'user'` predicate
			// in the UI — never have to special-case empty.
			role = "user"
		}
		s.msgs = append(s.msgs, Message{
			Timestamp:      e.Timestamp,
			Model:          e.Model,
			Provider:       e.Provider,
			Status:         e.Status,
			Role:           role,
			Prompt:         e.Prompt,
			PromptTemplate: e.PromptTemplate,
		})
		if len(s.msgs) > perSessionLimit {
			s.msgs = s.msgs[len(s.msgs)-perSessionLimit:]
		}
		return true
	}); err != nil {
		return nil, err
	}

	groups := make([]CWDGroup, 0, len(byCWD))
	for cwd, c := range byCWD {
		sessions := make([]Session, 0, len(c.sessions))
		for sid, s := range c.sessions {
			sessions = append(sessions, Session{
				SessionID:     sid,
				Client:        s.client,
				ClientVersion: s.clientVersion,
				Models:        sortedSetKeys(s.models),
				FirstSeen:     s.firstSeen,
				LastSeen:      s.lastSeen,
				MessageCount:  s.total,
				Truncated:     s.total > len(s.msgs),
				Messages:      s.msgs,
			})
		}
		sort.SliceStable(sessions, func(i, j int) bool {
			return sessions[i].LastSeen.After(sessions[j].LastSeen)
		})
		groups = append(groups, CWDGroup{
			CWD:          cwd,
			MessageCount: c.msgCount,
			LastSeen:     c.lastSeen,
			Sessions:     sessions,
		})
	}
	sort.SliceStable(groups, func(i, j int) bool {
		return groups[i].LastSeen.After(groups[j].LastSeen)
	})

	return &Detail{
		KeyHash:       keyHash,
		KeyHint:       configuredHint,
		Configured:    configured,
		TotalMessages: totalMessages,
		TotalSessions: len(totalSessions),
		TotalCWDs:     len(byCWD),
		Groups:        groups,
	}, nil
}

// MakeKeyHint returns "abcd...wxyz" — the head and tail of an API key — so
// the UI can display a recognizable token without leaking the full secret.
// Short keys (≤ 8 chars) are returned verbatim because there's nothing to mask.
func MakeKeyHint(k string) string {
	if len(k) <= 8 {
		return k
	}
	return k[:4] + "..." + k[len(k)-4:]
}

// IsHexKeyHash reports whether s is exactly the format produced by
// ratelimit.HashKey (12 lowercase hex chars). Used by handlers to decide
// whether an inbound :key path parameter is already a hash or a raw key
// that needs hashing.
func IsHexKeyHash(s string) bool {
	if len(s) != 12 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isLower := c >= 'a' && c <= 'f'
		isDigit := c >= '0' && c <= '9'
		if !isLower && !isDigit {
			return false
		}
	}
	return true
}

func sortedSetKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
