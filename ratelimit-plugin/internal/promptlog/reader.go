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
//
// Emits a debug log line with file count + duration so operators can spot
// slow installs before users complain. Cost is O(all lines × dir) per call;
// pagination at the API layer reduces payload but not scan time.
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
	start := time.Now()
	for _, path := range files {
		if !scanFile(path, fn) {
			log.Debugf("promptlog: scan dir=%s files=%d (stopped early) dur=%s", dir, len(files), time.Since(start))
			return nil
		}
	}
	log.Debugf("promptlog: scan dir=%s files=%d dur=%s", dir, len(files), time.Since(start))
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
	// SessionCount is the total number of sessions in this CWD, regardless of
	// any cursor filter or session_limit cap applied to Sessions. Stable
	// across initial-load and load-more responses so the UI can render
	// "X loaded of Y total" without bookkeeping.
	SessionCount int `json:"session_count"`
	// HasMore reports whether there are sessions older than the last one in
	// Sessions that would be returned by a follow-up load-more page with the
	// same session_limit. Distinct from SessionCount because session_before
	// shifts the window.
	HasMore bool `json:"has_more"`
	// Sessions is the windowed/paginated slice for this response. Always a
	// non-nil slice — empty when lazy (overview mode past initialCWDs) or
	// when headers_only=1. Marshaling nil here would emit `null` and break
	// `group.sessions.length` on the TS side.
	Sessions []Session `json:"sessions"`
}

// SessionCursor is the composite pagination key used by session_before.
// Strict timestamp-only comparison drops sessions sharing the exact same
// last_seen (cheap to hit when sessions are bursty), so we tie-break on
// session_id with deterministic ordering (lexicographic ascending) so the
// caller can resume safely.
type SessionCursor struct {
	Ts  time.Time
	Sid string
}

// DetailOpts bundles the optional knobs for BuildDetail. Zero values mean
// "no filter / use default". MessageLimit defaults to 200, SessionLimit to
// 200, InitialCWDs to 20.
type DetailOpts struct {
	// MessageLimit caps messages per session (existing `limit` query param).
	MessageLimit int
	// SessionLimit caps sessions per CWD in this response.
	SessionLimit int
	// InitialCWDs caps the number of CWDs (sorted by last_seen desc) that
	// get their Sessions inlined. CWDs past this index get an empty Sessions
	// slice and HasMore=true so the client lazy-loads on expand. Ignored
	// when CWDFilter is set (we only return that one CWD).
	InitialCWDs int
	// CWDFilter, when non-empty, restricts the scan to entries whose CWD
	// matches exactly. The response then contains at most one CWDGroup.
	CWDFilter string
	// SessionBefore, when non-nil, drops sessions whose ordering key is not
	// strictly older than the cursor. Only meaningful with CWDFilter set
	// (the handler rejects otherwise).
	SessionBefore *SessionCursor
	// SessionFilter, when non-empty, restricts the response Sessions
	// array to just the matching session. CWD-level meta (SessionCount,
	// MessageCount) is still computed from the full CWD aggregate so the
	// caller can still display "1 of N sessions". Requires CWDFilter.
	SessionFilter string
	// MessageBefore, when non-zero, drops messages with timestamp not
	// strictly older. Used to page older messages within a single session.
	// Requires SessionFilter (cursor is meaningless across sessions: two
	// sessions can share a timestamp). Tied per-session timestamps are
	// rare in practice but if two messages share ts exactly, the older
	// one may be lost on the page boundary — documented limitation.
	MessageBefore time.Time
	// HeadersOnly leaves every group's Sessions as an empty slice while
	// still computing SessionCount and HasMore from the full aggregate.
	// Used by the refresh button so it can update meta without clobbering
	// already-loaded session pages on the client.
	HeadersOnly bool
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
// tree grouped by cwd → session.
//
// Pagination model (opts):
//   - MessageLimit caps messages per session (sliding window, most recent
//     N kept). Sessions exceeding the cap are flagged Truncated.
//   - SessionLimit caps the number of sessions returned per CWDGroup.
//     SessionCount holds the full per-CWD total (unaffected by SessionBefore)
//     so the UI can show "X of Y" without bookkeeping.
//   - CWDFilter scopes the scan: when set, only entries matching that CWD
//     are aggregated and at most one group is returned. TotalMessages /
//     TotalSessions / TotalCWDs then reflect just the filtered CWD —
//     intentional, so a CWD-scoped reload reports CWD-scoped totals.
//   - SessionBefore is a composite (last_seen, session_id) cursor; a
//     session passes iff its (last_seen, session_id) is strictly older.
//     Plain timestamp comparison would drop tied last_seen sessions.
//   - InitialCWDs (only in overview mode, CWDFilter empty) decides how
//     many CWDs (sorted last_seen desc) get their Sessions populated;
//     the rest get an empty slice + HasMore=true for lazy load.
//   - HeadersOnly clears every group's Sessions to an empty slice after
//     SessionCount/HasMore have been computed — for non-destructive
//     refresh that doesn't clobber already-loaded pages on the client.
//
// Empty Sessions is always `[]Session{}`, never nil, so JSON marshals it
// as `[]` (not `null`) and TS callers can rely on `.length`.
func BuildDetail(dir, keyHash string, configuredHint string, configured bool, opts DetailOpts) (*Detail, error) {
	if opts.MessageLimit <= 0 {
		opts.MessageLimit = 200
	}
	if opts.SessionLimit <= 0 {
		opts.SessionLimit = 200
	}
	if opts.InitialCWDs < 0 {
		opts.InitialCWDs = 0
	} else if opts.InitialCWDs == 0 && opts.CWDFilter == "" {
		// 0 from the handler means "param not provided" → use default. A
		// caller that really wants zero CWDs inlined would pass a tiny
		// MessageLimit instead; this branch is the natural ergonomics.
		opts.InitialCWDs = 20
	}
	type sessAgg struct {
		client        string
		clientVersion string
		models        map[string]struct{}
		firstSeen     time.Time
		lastSeen      time.Time
		msgs          []Message
		// total counts every entry for this session, regardless of filters.
		// Drives MessageCount in the response so the UI can show "X of Y"
		// even when a cursor narrows the returned window.
		total int
		// eligibleCount counts entries that survived MessageBefore filtering.
		// Truncated is `eligibleCount > len(msgs)` — answers "is there an
		// older page to fetch with this cursor?" When no MessageBefore is
		// set, eligibleCount == total, so semantics are unchanged.
		eligibleCount int
	}
	type cwdAgg struct {
		sessions map[string]*sessAgg
		msgCount int
		lastSeen time.Time
	}
	byCWD := map[string]*cwdAgg{}
	totalSessions := map[string]struct{}{}
	totalMessages := 0

	if err := scanAll(dir, func(e Entry) bool {
		if e.KeyHash != keyHash {
			return true
		}
		cwd := e.CWD
		if cwd == "" {
			cwd = "(unknown)"
		}
		// Short-circuit when the caller scoped to one CWD: this is the load-
		// more / refresh-cwd path and we don't want to spend any aggregate
		// work on neighboring CWDs.
		if opts.CWDFilter != "" && cwd != opts.CWDFilter {
			return true
		}
		totalMessages++
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
		// Skip the per-message slice append on the headers_only fast
		// path — we only need counts/timestamps for that response, and
		// the slice append (with its periodic re-slice for the sliding
		// window) is the hot allocation in this scan.
		if !opts.HeadersOnly {
			// SessionFilter narrows the response to one session, so we
			// don't need to allocate Messages for sessions we'll discard
			// when building groups. CWD-level counters (msgCount,
			// lastSeen, SessionCount via len(c.sessions)) still get
			// updated above so the UI's "session 1 of N" stays accurate.
			if opts.SessionFilter != "" && sid != opts.SessionFilter {
				return true
			}
			// MessageBefore is the message-page cursor: only entries
			// strictly older count. eligibleCount tracks how many
			// survived; Truncated below uses it instead of s.total.
			if !opts.MessageBefore.IsZero() && !e.Timestamp.Before(opts.MessageBefore) {
				return true
			}
			s.eligibleCount++
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
			if len(s.msgs) > opts.MessageLimit {
				s.msgs = s.msgs[len(s.msgs)-opts.MessageLimit:]
			}
		}
		return true
	}); err != nil {
		return nil, err
	}

	groups := make([]CWDGroup, 0, len(byCWD))
	for cwd, c := range byCWD {
		// Fast path: headers_only doesn't need the Session list at all —
		// just SessionCount + meta. Skipping the alloc + sort + cursor
		// filter is the difference between O(num_sessions) and O(1) per
		// CWD; on a power user's history that's hundreds of allocations
		// avoided every time they hit refresh.
		if opts.HeadersOnly {
			groups = append(groups, CWDGroup{
				CWD:          cwd,
				MessageCount: c.msgCount,
				LastSeen:     c.lastSeen,
				SessionCount: len(c.sessions),
				HasMore:      len(c.sessions) > 0,
				Sessions:     []Session{},
			})
			continue
		}
		// Build the full session list for this CWD, then sort and apply
		// cursor/cap windowing. Sort key is (LastSeen desc, SessionID asc)
		// so the cursor can resume deterministically across tied last_seen.
		allSessions := make([]Session, 0, len(c.sessions))
		for sid, s := range c.sessions {
			// SessionFilter: skip sessions we don't intend to emit so the
			// downstream sort + cursor work doesn't touch them.
			if opts.SessionFilter != "" && sid != opts.SessionFilter {
				continue
			}
			allSessions = append(allSessions, Session{
				SessionID:     sid,
				Client:        s.client,
				ClientVersion: s.clientVersion,
				Models:        sortedSetKeys(s.models),
				FirstSeen:     s.firstSeen,
				LastSeen:      s.lastSeen,
				MessageCount:  s.total,
				// Truncated answers "is there an older page to fetch?"
				// — uses eligibleCount so a load-more response with no
				// older messages reports Truncated=false even when the
				// session's full history (s.total) exceeds the limit.
				Truncated: s.eligibleCount > len(s.msgs),
				Messages:  s.msgs,
			})
		}
		sort.SliceStable(allSessions, func(i, j int) bool {
			if allSessions[i].LastSeen.Equal(allSessions[j].LastSeen) {
				return allSessions[i].SessionID < allSessions[j].SessionID
			}
			return allSessions[i].LastSeen.After(allSessions[j].LastSeen)
		})

		// SessionCount reflects the FULL CWD session population —
		// SessionFilter narrowed allSessions to one entry, but the UI
		// still wants the denominator for "1 of N sessions". Falling
		// back to len(c.sessions) is exact because every session that
		// was scanned ended up in c.sessions (the filter only skips the
		// Sessions[] slice, not the aggregate map).
		sessionCount := len(c.sessions)

		// Cursor filter: keep sessions strictly older than the cursor on
		// (LastSeen, SessionID). Using `<=` + dedup-by-id would be fragile
		// across pages — composite is the right shape.
		filtered := allSessions
		if opts.SessionBefore != nil {
			cur := opts.SessionBefore
			filtered = filtered[:0]
			for _, s := range allSessions {
				if s.LastSeen.Before(cur.Ts) || (s.LastSeen.Equal(cur.Ts) && s.SessionID > cur.Sid) {
					filtered = append(filtered, s)
				}
			}
		}

		hasMore := len(filtered) > opts.SessionLimit
		if len(filtered) > opts.SessionLimit {
			filtered = filtered[:opts.SessionLimit]
		}

		groups = append(groups, CWDGroup{
			CWD:          cwd,
			MessageCount: c.msgCount,
			LastSeen:     c.lastSeen,
			SessionCount: sessionCount,
			HasMore:      hasMore,
			Sessions:     filtered,
		})
	}
	sort.SliceStable(groups, func(i, j int) bool {
		return groups[i].LastSeen.After(groups[j].LastSeen)
	})

	// Overview mode: only the first InitialCWDs groups carry inlined
	// sessions. The rest are made lazy (empty slice + has_more derived from
	// SessionCount) so the initial payload doesn't blow up on power users
	// with dozens of projects. Skipped when HeadersOnly is set — that case
	// already shipped sessions=[] from the fast path above.
	if opts.CWDFilter == "" && !opts.HeadersOnly {
		for i := range groups {
			if i >= opts.InitialCWDs {
				groups[i].Sessions = []Session{}
				groups[i].HasMore = groups[i].SessionCount > 0
			}
		}
	}

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
