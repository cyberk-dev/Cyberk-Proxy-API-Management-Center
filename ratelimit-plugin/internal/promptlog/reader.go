package promptlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	log "github.com/sirupsen/logrus"

	"github.com/cyberk/ratelimit-plugin/internal/ratelimit"
)

// scanCallback is invoked once per JSONL line; returning false stops scanning
// early. Errors parsing individual lines are swallowed (logged at debug) so
// one corrupt entry doesn't abort the whole report.
type scanCallback func(Entry) bool

// parentRef holds the render-time (cwd, sid) bucket a subagent should merge
// into. Materialized by callers walking entries for one key — the absence of
// a parent for a flagged subagent entry is what marks it as orphan.
type parentRef struct {
	cwd string
	sid string
}

// bucketFor resolves an entry's render-time bucket. Three outcomes:
//   - Plain user/assistant turn (no subagent markers): bucket is the entry's own
//     (cwd, sid). isSub=false, isOrphan=false.
//   - Subagent with a known parent (Claude Code shares SessionID; opencode uses
//     ParentSessionID → parent's SessionID): bucket inherits from parent so the
//     subagent renders inside the parent's session card. isSub=true.
//   - Subagent whose parent rolled out of retention: isOrphan=true. Callers
//     drop these — rendering them standalone surfaces dispatcher framing
//     ("Spawn a subagent to…") with no human context, which is pure noise.
//
// The on-disk Entry is never rewritten; this is a pure read-side decision so
// a later scan with the parent back in scope can re-link the same entry.
func bucketFor(e Entry, parents map[string]parentRef) (cwd, sid string, isSub bool, subID string, isOrphan bool) {
	cwd, sid = e.CWD, e.SessionID
	switch {
	case e.AgentID != "" && e.SessionID != "":
		if p, ok := parents[e.SessionID]; ok {
			cwd = p.cwd
			isSub, subID = true, shortAgentID(e.AgentID)
		}
	case e.ParentSessionID != "":
		if p, ok := parents[e.ParentSessionID]; ok {
			cwd, sid = p.cwd, p.sid
			isSub, subID = true, shortSessionID(e.SessionID)
		}
	}
	if (e.AgentID != "" || e.ParentSessionID != "") && !isSub {
		isOrphan = true
	}
	return
}

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
//
// Reads disk on every call; the in-memory Index serves the same shape via
// Index.ListUsers without a scan. This top-level function stays so the
// equivalence test (index_test.go) can run both paths against the same
// fixtures.
func ListUsers(dir string, configuredKeys []string) ([]UserSummary, error) {
	perKey := map[string][]Entry{}
	if err := scanAll(dir, func(e Entry) bool {
		perKey[e.KeyHash] = append(perKey[e.KeyHash], e)
		return true
	}); err != nil {
		return nil, err
	}
	return aggregateUsers(perKey, configuredKeys), nil
}

// aggregateUsers is the pure-data aggregation shared by the scan-based
// ListUsers and the in-memory Index.ListUsers. perKey maps key_hash to that
// key's entries in chronological-insertion order. Configured keys with zero
// activity are unioned in at the tail so brand-new keys still surface in
// the UI.
func aggregateUsers(perKey map[string][]Entry, configuredKeys []string) []UserSummary {
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

	for hash, entries := range perKey {
		for _, e := range entries {
			a := byHash[hash]
			if a == nil {
				a = &agg{
					sessions:  make(map[string]struct{}),
					cwds:      make(map[string]struct{}),
					clients:   make(map[string]struct{}),
					models:    make(map[string]struct{}),
					firstSeen: e.Timestamp,
					lastSeen:  e.Timestamp,
				}
				byHash[hash] = a
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
		}
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
	for _, k := range configuredKeys {
		hash := ratelimit.HashKey(k)
		if _, ok := byHash[hash]; ok {
			continue
		}
		out = append(out, UserSummary{KeyHash: hash, KeyHint: MakeKeyHint(k), Configured: true})
	}

	sort.SliceStable(out, func(i, j int) bool {
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
	return out
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

// SearchHit is one row of /v0/management/prompts/users/:key/search. CWD and
// SessionID are the RENDER-TIME bucket (subagent → parent, same as
// BuildDetail), so a click can deep-link into the parent's reading pane
// without the UI repeating that resolution. Orphan subagents are dropped
// (would surface dispatcher framing with no human context — pure noise),
// matching BuildDetail's behavior so search results stay consistent with
// what the tree shows.
type SearchHit struct {
	CWD        string    `json:"cwd"`
	SessionID  string    `json:"session_id"`
	Timestamp  time.Time `json:"ts"`
	Role       string    `json:"role,omitempty"`
	// Excerpt is a clipped window of Message.Prompt around the first match,
	// whitespace normalized, with "…" prefixes/suffixes when clipped. The
	// caller re-locates the match with a case-insensitive indexOf on this
	// string to render highlights; we don't return byte offsets because JS
	// strings index by UTF-16 code units while Go bytes are UTF-8 — the
	// translation is error-prone and the client-side re-scan is cheap.
	Excerpt    string `json:"excerpt"`
	IsSubagent bool   `json:"is_subagent,omitempty"`
	SubagentID string `json:"subagent_id,omitempty"`
}

// SearchResult is the response shape for the search endpoint. TotalMatches
// counts every match for this query (the scan completes regardless of
// limit), while Matches is the limit-capped, ts-desc-sorted slice. Truncated
// reports whether the slice was clipped.
type SearchResult struct {
	Matches      []SearchHit `json:"matches"`
	TotalMatches int         `json:"total_matches"`
	Truncated    bool        `json:"truncated"`
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
//
// IsSubagent / SubagentID are set during the second-pass bucketing in
// BuildDetail when an entry has been linked to a parent (Claude Code via
// shared SessionID + AgentID; opencode via ParentSessionID). The UI uses
// them to render the row indented under its dispatching parent. Orphan
// subagent entries (no parent in retention) leave both at zero values and
// render as ordinary messages — the on-disk Entry still carries AgentID /
// ParentSessionID so a later scan can re-link them once the parent is in
// scope again.
type Message struct {
	Timestamp      time.Time `json:"ts"`
	Model          string    `json:"model,omitempty"`
	Provider       string    `json:"provider,omitempty"`
	Status         int       `json:"status"`
	Role           string    `json:"role,omitempty"`
	Prompt         string    `json:"prompt"`
	PromptTemplate string    `json:"prompt_template,omitempty"`
	Blocks         []Block   `json:"blocks,omitempty"`
	IsSubagent     bool      `json:"is_subagent,omitempty"`
	SubagentID     string    `json:"subagent_id,omitempty"`
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
//
// Reads disk on every call. Index.BuildDetail serves the same shape from
// RAM; this top-level function stays so the equivalence test
// (index_test.go) can run both paths against the same fixtures.
func BuildDetail(dir, keyHash string, configuredHint string, configured bool, opts DetailOpts) (*Detail, error) {
	entries := make([]Entry, 0, 256)
	if err := scanAll(dir, func(e Entry) bool {
		if e.KeyHash != keyHash {
			return true
		}
		// Blocks is the heavy field (image metadata, tool refs, base64
		// fingerprints) and the tree response never includes it. Drop
		// before materializing.
		e.Blocks = nil
		entries = append(entries, e)
		return true
	}); err != nil {
		return nil, err
	}
	return aggregateDetail(entries, keyHash, configuredHint, configured, opts), nil
}

// aggregateDetail is the pure aggregation step shared by the scan-based
// BuildDetail and Index.BuildDetail. entries is the chronological-insertion-
// ordered slice of Entries for one key; this function derives the parent
// session map from them, then runs the bucket/window/pagination pipeline.
func aggregateDetail(entries []Entry, keyHash, configuredHint string, configured bool, opts DetailOpts) *Detail {
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
		// total counts every entry routed into this aggregate, regardless of
		// filters. Drives MessageCount in the response so the UI can show
		// "X of Y" even when a cursor narrows the returned window. For
		// opencode subagent merges, this INCLUDES the subagent turns merged
		// into the parent's session — the UI's "Nm" badge intentionally
		// reflects the visible conversation length, subagent rows included.
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

	// Derive parent-session map. Last-writer-wins is fine: multiple parent
	// turns in the same session must agree on CWD (the env block doesn't
	// change mid-session), so the final stored value is just the most
	// recent observation. Entries must be in chronological order for the
	// "last" to be deterministic — both callers preserve that ordering.
	parentBySession := map[string]parentRef{}
	for _, e := range entries {
		if e.AgentID == "" && e.ParentSessionID == "" && e.CWD != "" && e.SessionID != "" {
			parentBySession[e.SessionID] = parentRef{cwd: e.CWD, sid: e.SessionID}
		}
	}

	for _, e := range entries {
		// Bucket decision: resolve subagent → parent, else passthrough.
		// Raw e.CWD / e.SessionID stay untouched on disk — bucketCWD /
		// bucketSID are the render-time keys only. See bucketFor for the
		// full decision table including orphan-subagent semantics.
		bucketCWD, bucketSID, isSub, subID, isOrphan := bucketFor(e, parentBySession)
		if isOrphan {
			continue
		}

		cwd := bucketCWD
		if cwd == "" {
			cwd = "(unknown)"
		}
		if opts.CWDFilter != "" && cwd != opts.CWDFilter {
			continue
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

		sid := bucketSID
		if sid == "" {
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
		if opts.HeadersOnly {
			continue
		}
		if opts.SessionFilter != "" && sid != opts.SessionFilter {
			continue
		}
		if !opts.MessageBefore.IsZero() && !e.Timestamp.Before(opts.MessageBefore) {
			continue
		}
		s.eligibleCount++
		role := e.Role
		if role == "" {
			role = "user"
		}
		s.msgs = append(s.msgs, Message{
			Timestamp:      e.Timestamp,
			Model:          e.Model,
			Provider:       e.Provider,
			Status:         e.Status,
			Role:           role,
			Prompt:         stripCommandWrapperPrefix(e.Prompt),
			PromptTemplate: e.PromptTemplate,
			IsSubagent:     isSub,
			SubagentID:     subID,
		})
		if len(s.msgs) > opts.MessageLimit {
			s.msgs = s.msgs[len(s.msgs)-opts.MessageLimit:]
		}
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
	}
}

// SearchMessages does a case-insensitive substring scan over Message.Prompt
// for entries belonging to keyHash and returns ts-desc, limit-capped hits.
// Caller validates query is non-empty after trim; passing "" yields an empty
// result (no match) rather than every message.
//
// Subagent linking mirrors BuildDetail's 2-pass model so a click on a search
// hit can route into the parent's reading pane the same way the tree does.
// Orphan subagents (parent rolled out of retention) are dropped — they'd
// otherwise show dispatcher framing without human context, matching the
// tree's behavior for consistency.
//
// Template-prefix search is OUT OF SCOPE for PR1: a templated entry's full
// text is `template.body + prompt_suffix`, and the body lives in a separate
// store. This function searches the suffix only. Documented limitation: a
// query that would only match inside the template prefix won't hit. Wire
// the TemplateStore in here later if that gap matters.
func SearchMessages(dir, keyHash, query string, limit int) (*SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return &SearchResult{Matches: []SearchHit{}}, nil
	}
	entries := make([]Entry, 0, 256)
	if err := scanAll(dir, func(e Entry) bool {
		if e.KeyHash != keyHash {
			return true
		}
		e.Blocks = nil
		entries = append(entries, e)
		return true
	}); err != nil {
		return nil, err
	}
	return aggregateSearch(entries, query, limit), nil
}

// aggregateSearch is the pure search step shared by the scan-based
// SearchMessages and Index.SearchMessages. Empty/whitespace query returns
// an empty result (callers normally pre-validate, but the guard here keeps
// the function safe to call directly).
func aggregateSearch(entries []Entry, query string, limit int) *SearchResult {
	q := strings.TrimSpace(query)
	if q == "" {
		return &SearchResult{Matches: []SearchHit{}}
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	// Pre-lowercase the query once (rune-aware) so the per-entry compare
	// path only allocates the haystack lowercase.
	lowQuery := []rune(strings.ToLower(q))

	// Derive parent map from entries; same chronological-order assumption
	// as aggregateDetail.
	parentBySession := map[string]parentRef{}
	for _, e := range entries {
		if e.AgentID == "" && e.ParentSessionID == "" && e.CWD != "" && e.SessionID != "" {
			parentBySession[e.SessionID] = parentRef{cwd: e.CWD, sid: e.SessionID}
		}
	}

	hits := make([]SearchHit, 0, 64)
	for _, e := range entries {
		if e.Prompt == "" {
			continue
		}
		bucketCWD, bucketSID, isSub, subID, isOrphan := bucketFor(e, parentBySession)
		if isOrphan {
			continue
		}

		excerpt := buildExcerpt(e.Prompt, lowQuery, 60)
		if excerpt == "" {
			continue
		}

		cwd := bucketCWD
		if cwd == "" {
			cwd = "(unknown)"
		}
		sid := bucketSID
		if sid == "" {
			sid = "(no-session)"
		}

		hits = append(hits, SearchHit{
			CWD:        cwd,
			SessionID:  sid,
			Timestamp:  e.Timestamp,
			Role:       e.Role,
			Excerpt:    excerpt,
			IsSubagent: isSub,
			SubagentID: subID,
		})
	}

	total := len(hits)
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].Timestamp.After(hits[j].Timestamp)
	})
	truncated := total > limit
	if truncated {
		hits = hits[:limit]
	}
	return &SearchResult{
		Matches:      hits,
		TotalMatches: total,
		Truncated:    truncated,
	}
}

// buildExcerpt returns a ±window-rune slice of prompt around the first
// case-insensitive match of lowQuery (already lowercased and rune-sliced by
// the caller), with whitespace collapsed to single spaces and "…" markers on
// any clipped end. Empty result means no match.
//
// Rune-aware throughout: byte-level strings.Index would break on multi-byte
// runes (Vietnamese diacritics, emoji), and strings.ToLower can change byte
// counts (e.g. some Turkish forms), so byte offsets returned by Index don't
// round-trip back to the original string safely. unicode.ToLower is rune-to-
// rune so the two slices we build below stay positionally aligned.
func buildExcerpt(prompt string, lowQuery []rune, window int) string {
	if len(lowQuery) == 0 {
		return ""
	}
	runes := []rune(prompt)
	if len(runes) < len(lowQuery) {
		return ""
	}
	lowRunes := make([]rune, len(runes))
	for i, r := range runes {
		lowRunes[i] = unicode.ToLower(r)
	}
	idx := -1
outer:
	for i := 0; i <= len(lowRunes)-len(lowQuery); i++ {
		for j := 0; j < len(lowQuery); j++ {
			if lowRunes[i+j] != lowQuery[j] {
				continue outer
			}
		}
		idx = i
		break
	}
	if idx < 0 {
		return ""
	}

	start := idx - window
	end := idx + len(lowQuery) + window
	if start < 0 {
		start = 0
	}
	if end > len(runes) {
		end = len(runes)
	}

	var sb strings.Builder
	sb.Grow(end - start + 4)
	if start > 0 {
		sb.WriteRune('…')
	}
	lastWasSpace := false
	for _, r := range runes[start:end] {
		// Normalize whitespace to a single space so excerpts render on one
		// line in the UI without the page wrapping logic having to fight a
		// stray newline or tab from inside the prompt.
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		if r == ' ' {
			if !lastWasSpace {
				sb.WriteRune(' ')
				lastWasSpace = true
			}
			continue
		}
		sb.WriteRune(r)
		lastWasSpace = false
	}
	if end < len(runes) {
		sb.WriteRune('…')
	}
	return strings.TrimSpace(sb.String())
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

// stripCommandWrapperPrefix removes Claude Code's chained slash-command
// preamble from the start of a prompt. When a user types e.g.
// `/clear` followed by a real question, the CLI ships the request body
// with a leading text block containing:
//
//	<command-name>/clear</command-name>
//	<command-message>clear</command-message>
//	<command-args></command-args>
//
// extract.go's isWrapperOnly drops blocks enclosed by a SINGLE wrapper
// tag, but the chained form here starts with <command-name> and ends
// with </command-args> — no single tag matches, so the wrapper lands
// on disk verbatim and clutters the first-message preview in the UI.
//
// This helper runs read-time so historical entries also display clean;
// disk content is untouched. Conservative: bails at the first
// non-wrapper, non-whitespace character, so user content that legitimately
// mentions one of these tags later in the prompt is preserved. Returns
// the input unchanged if nothing matches.
func stripCommandWrapperPrefix(s string) string {
	rest := strings.TrimLeft(s, " \t\r\n")
	if !strings.HasPrefix(rest, "<command-") {
		return s
	}
	changed := false
	for strings.HasPrefix(rest, "<command-") {
		end := strings.IndexByte(rest, '>')
		if end < 0 {
			break
		}
		tag := rest[1:end]
		if !isCommandWrapperTag(tag) {
			break
		}
		closing := "</" + tag + ">"
		closeAt := strings.Index(rest, closing)
		if closeAt < 0 {
			break
		}
		rest = rest[closeAt+len(closing):]
		rest = strings.TrimLeft(rest, " \t\r\n")
		changed = true
	}
	if !changed {
		return s
	}
	return rest
}

func isCommandWrapperTag(tag string) bool {
	switch tag {
	case "command-name", "command-message", "command-args",
		"command-stdout", "command-stderr":
		return true
	}
	return false
}

// shortAgentID returns a recognizable prefix of a Claude Code agent id
// (observed shape: 17-hex). 8 chars is enough to disambiguate concurrent
// subagent runs in a single session without overwhelming the chip.
func shortAgentID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// shortSessionID returns the tail of an opencode session id, dropping the
// "ses_" prefix when present so the chip shows the meaningful entropy
// instead of a constant string. Falls back to the head if the id is shorter
// than expected.
func shortSessionID(id string) string {
	s := strings.TrimPrefix(id, "ses_")
	if len(s) <= 8 {
		return s
	}
	return s[len(s)-8:]
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
