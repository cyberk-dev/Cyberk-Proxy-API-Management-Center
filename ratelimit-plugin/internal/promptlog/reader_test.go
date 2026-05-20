package promptlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyberk/ratelimit-plugin/internal/ratelimit"
)

// writeJSONL is a test helper that writes one Entry per call so individual
// assertions read like a tiny scenario script.
func writeJSONL(t *testing.T, dir, date string, entries ...Entry) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "prompts-"+date+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, e := range entries {
		b, _ := json.Marshal(e)
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListUsers_EmptyDir(t *testing.T) {
	users, err := ListUsers(t.TempDir(), []string{"sk-alice", "sk-bob"})
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 (configured-only) users, got %d", len(users))
	}
	for _, u := range users {
		if !u.Configured || u.MessageCount != 0 {
			t.Errorf("expected empty-configured row, got %+v", u)
		}
	}
}

func TestListUsers_Aggregates(t *testing.T) {
	dir := t.TempDir()
	aliceHash := ratelimit.HashKey("sk-alice")
	bobHash := ratelimit.HashKey("sk-bob")
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)

	writeJSONL(t, dir, "2026-05-15",
		Entry{KeyHash: aliceHash, Timestamp: now, SessionID: "s1", CWD: "/proj", Client: "claude_code", Model: "claude-opus", Provider: "anthropic"},
		Entry{KeyHash: aliceHash, Timestamp: now.Add(time.Hour), SessionID: "s1", CWD: "/proj", Client: "claude_code", Model: "claude-opus", Provider: "anthropic"},
		Entry{KeyHash: aliceHash, Timestamp: now.Add(2 * time.Hour), SessionID: "s2", CWD: "/proj2", Client: "claude_code", Model: "claude-haiku", Provider: "anthropic"},
		Entry{KeyHash: bobHash, Timestamp: now.Add(30 * time.Minute), SessionID: "x", CWD: "/proj", Client: "opencode"},
	)

	users, err := ListUsers(dir, []string{"sk-alice", "sk-bob"})
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("got %d users", len(users))
	}

	// Alice has 3 msgs across 2 sessions / 2 cwds → most recent activity.
	if users[0].KeyHash != aliceHash {
		t.Errorf("expected alice first, got %s", users[0].KeyHash)
	}
	if users[0].MessageCount != 3 || users[0].SessionCount != 2 || users[0].CWDCount != 2 {
		t.Errorf("alice agg wrong: %+v", users[0])
	}
	if len(users[0].Models) != 2 {
		t.Errorf("alice models: %v", users[0].Models)
	}
}

func TestListUsers_OrphanedKey(t *testing.T) {
	// Activity from a key that is no longer in config should still appear
	// (Configured=false) so operators can spot orphaned tokens.
	dir := t.TempDir()
	ghostHash := ratelimit.HashKey("sk-ghost")
	writeJSONL(t, dir, "2026-05-15",
		Entry{KeyHash: ghostHash, Timestamp: time.Now(), SessionID: "g", CWD: "/x"},
	)
	users, err := ListUsers(dir, []string{"sk-alice"})
	if err != nil {
		t.Fatal(err)
	}
	var ghost *UserSummary
	for i := range users {
		if users[i].KeyHash == ghostHash {
			ghost = &users[i]
		}
	}
	if ghost == nil || ghost.Configured {
		t.Fatalf("expected orphaned ghost, got %+v", ghost)
	}
}

func TestBuildDetail_GroupsByCwdAndSession(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-alice")
	t0 := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)

	writeJSONL(t, dir, "2026-05-15",
		Entry{KeyHash: hash, Timestamp: t0, CWD: "/proj", SessionID: "s1", Client: "claude_code", Model: "opus", Prompt: "hi"},
		Entry{KeyHash: hash, Timestamp: t0.Add(time.Minute), CWD: "/proj", SessionID: "s1", Client: "claude_code", Model: "opus", Prompt: "again"},
		Entry{KeyHash: hash, Timestamp: t0.Add(time.Hour), CWD: "/proj2", SessionID: "s2", Client: "amp", Model: "haiku", Prompt: "other"},
	)

	detail, err := BuildDetail(dir, hash, "sk-a...lice", true, DetailOpts{MessageLimit: 200, SessionLimit: 200, InitialCWDs: 20})
	if err != nil {
		t.Fatal(err)
	}
	if detail.TotalMessages != 3 || detail.TotalSessions != 2 || detail.TotalCWDs != 2 {
		t.Fatalf("counts wrong: %+v", detail)
	}
	// /proj2 has the latest activity → first group.
	if detail.Groups[0].CWD != "/proj2" {
		t.Errorf("group order: %+v", detail.Groups)
	}
	if detail.Groups[1].Sessions[0].MessageCount != 2 {
		t.Errorf("s1 msg count: %+v", detail.Groups[1].Sessions[0])
	}
}

func TestBuildDetail_TruncatesPerSession(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-alice")
	base := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	var entries []Entry
	for i := 0; i < 50; i++ {
		entries = append(entries, Entry{
			KeyHash:   hash,
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			CWD:       "/p",
			SessionID: "s",
			Prompt:    "msg",
		})
	}
	writeJSONL(t, dir, "2026-05-15", entries...)

	detail, err := BuildDetail(dir, hash, "", false, DetailOpts{MessageLimit: 10, SessionLimit: 200, InitialCWDs: 20})
	if err != nil {
		t.Fatal(err)
	}
	sess := detail.Groups[0].Sessions[0]
	if !sess.Truncated || len(sess.Messages) != 10 || sess.MessageCount != 50 {
		t.Errorf("truncation wrong: %+v", sess)
	}
	// Keep the MOST RECENT — last entry's timestamp should be in the window.
	last := sess.Messages[len(sess.Messages)-1].Timestamp
	if !last.Equal(base.Add(49 * time.Minute)) {
		t.Errorf("expected newest preserved, got %v", last)
	}
}

func TestIsHexKeyHash(t *testing.T) {
	cases := map[string]bool{
		"abcdef012345": true,
		"ABCDEF012345": false, // requires lower case
		"abcdef01234":  false, // wrong length
		"abcdef012345x": false,
		"sk-something": false,
	}
	for in, want := range cases {
		if got := IsHexKeyHash(in); got != want {
			t.Errorf("IsHexKeyHash(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMakeKeyHint(t *testing.T) {
	if MakeKeyHint("short") != "short" {
		t.Errorf("short key changed")
	}
	if MakeKeyHint("sk-abcdef-1234") != "sk-a...1234" {
		t.Errorf("long key hint: %q", MakeKeyHint("sk-abcdef-1234"))
	}
}

func TestBuildDetail_PreservesPromptTemplate(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-x")
	writeJSONL(t, dir, "2026-05-15", Entry{
		KeyHash:        hash,
		Timestamp:      time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC),
		CWD:            "/p",
		SessionID:      "s",
		Prompt:         " suffix tail",
		PromptTemplate: "abc123abc123",
	})
	detail, err := BuildDetail(dir, hash, "", false, DetailOpts{MessageLimit: 50, SessionLimit: 200, InitialCWDs: 20})
	if err != nil {
		t.Fatal(err)
	}
	m := detail.Groups[0].Sessions[0].Messages[0]
	if m.PromptTemplate != "abc123abc123" {
		t.Errorf("template hash got %q", m.PromptTemplate)
	}
	if m.Prompt != " suffix tail" {
		t.Errorf("prompt got %q", m.Prompt)
	}
}

func TestInlineTemplates_SplicesBody(t *testing.T) {
	store, _ := NewTemplateStore(t.TempDir())
	tpl, _ := store.Register("HEAD-BODY-", "test", time.Now())

	detail := &Detail{
		Groups: []CWDGroup{{
			Sessions: []Session{{
				Messages: []Message{
					{Prompt: "tail-A", PromptTemplate: tpl.Hash},
					{Prompt: "untemplated", PromptTemplate: ""},
					{Prompt: "tail-B", PromptTemplate: "deadbeefcafe"}, // unknown hash
				},
			}},
		}},
	}
	InlineTemplates(detail, store)
	msgs := detail.Groups[0].Sessions[0].Messages
	if msgs[0].Prompt != "HEAD-BODY-tail-A" || msgs[0].PromptTemplate != "" {
		t.Errorf("templated message not spliced: %+v", msgs[0])
	}
	if msgs[1].Prompt != "untemplated" {
		t.Errorf("non-templated touched: %+v", msgs[1])
	}
	if msgs[2].Prompt != "tail-B" || msgs[2].PromptTemplate != "deadbeefcafe" {
		t.Errorf("unknown-hash should be left as-is: %+v", msgs[2])
	}
}

func TestInlineTemplates_NilStoreSafe(t *testing.T) {
	detail := &Detail{Groups: []CWDGroup{{Sessions: []Session{{Messages: []Message{{Prompt: "x", PromptTemplate: "y"}}}}}}}
	InlineTemplates(detail, nil)
	if detail.Groups[0].Sessions[0].Messages[0].PromptTemplate != "y" {
		t.Errorf("nil store should not mutate")
	}
}

// seedSessions writes n sessions into the same CWD, each with one message.
// last_seen for session i is base + i*step, so session_id ordering and
// last_seen ordering both advance together. step=0 → all sessions tied.
func seedSessions(t *testing.T, dir, date, hash, cwd string, n int, base time.Time, step time.Duration) {
	t.Helper()
	entries := make([]Entry, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, Entry{
			KeyHash:   hash,
			Timestamp: base.Add(time.Duration(i) * step),
			CWD:       cwd,
			SessionID: "s-" + strconv.Itoa(i),
			Prompt:    "p",
		})
	}
	writeJSONL(t, dir, date, entries...)
}

func TestBuildDetail_SessionCapAndHasMore(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-a")
	base := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	seedSessions(t, dir, "2026-05-17", hash, "/p", 25, base, time.Minute)

	detail, err := BuildDetail(dir, hash, "", false, DetailOpts{
		MessageLimit: 200, SessionLimit: 10, InitialCWDs: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	g := detail.Groups[0]
	if g.SessionCount != 25 {
		t.Errorf("SessionCount=%d want 25", g.SessionCount)
	}
	if !g.HasMore {
		t.Error("HasMore=false but 25 > 10")
	}
	if len(g.Sessions) != 10 {
		t.Errorf("len(Sessions)=%d want 10", len(g.Sessions))
	}
	// Sorted last_seen desc → newest first.
	if g.Sessions[0].SessionID != "s-24" {
		t.Errorf("first session=%s want s-24", g.Sessions[0].SessionID)
	}
}

func TestBuildDetail_CompositeCursorTiedTimestamp(t *testing.T) {
	// Three sessions sharing the EXACT same last_seen. Strict timestamp-only
	// `<` would drop all of them when the cursor lands on any one of them;
	// composite (ts, sid) must let the resumed page see the older sids.
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-a")
	ts := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	writeJSONL(t, dir, "2026-05-17",
		Entry{KeyHash: hash, Timestamp: ts, CWD: "/p", SessionID: "s-a", Prompt: "x"},
		Entry{KeyHash: hash, Timestamp: ts, CWD: "/p", SessionID: "s-b", Prompt: "x"},
		Entry{KeyHash: hash, Timestamp: ts, CWD: "/p", SessionID: "s-c", Prompt: "x"},
	)

	// Page 1: SessionLimit=1, no cursor → newest tie-break wins (lowest sid
	// asc within tied last_seen). All three share last_seen so sort gives
	// (s-a, s-b, s-c); SessionLimit=1 → returns s-a.
	page1, err := BuildDetail(dir, hash, "", false, DetailOpts{
		MessageLimit: 200, SessionLimit: 1, CWDFilter: "/p",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.Groups) != 1 || len(page1.Groups[0].Sessions) != 1 {
		t.Fatalf("page1 unexpected: %+v", page1.Groups)
	}
	first := page1.Groups[0].Sessions[0].SessionID
	if first != "s-a" {
		t.Fatalf("page1 first=%s want s-a (tie-break sid asc)", first)
	}
	if !page1.Groups[0].HasMore {
		t.Error("page1 HasMore=false but 3 > 1")
	}

	// Page 2: cursor on (ts, s-a) — composite predicate accepts s-b and s-c
	// because their sid > "s-a" at the same ts. Strict ts-only would have
	// dropped both.
	page2, err := BuildDetail(dir, hash, "", false, DetailOpts{
		MessageLimit: 200, SessionLimit: 10, CWDFilter: "/p",
		SessionBefore: &SessionCursor{Ts: ts, Sid: "s-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, s := range page2.Groups[0].Sessions {
		got = append(got, s.SessionID)
	}
	if len(got) != 2 || got[0] != "s-b" || got[1] != "s-c" {
		t.Errorf("page2 sessions=%v want [s-b s-c]", got)
	}
	if page2.Groups[0].SessionCount != 3 {
		t.Errorf("page2 SessionCount=%d want 3 (full-CWD count, not filtered)", page2.Groups[0].SessionCount)
	}
}

func TestBuildDetail_SessionBeforeFilter(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-a")
	base := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	seedSessions(t, dir, "2026-05-17", hash, "/p", 5, base, time.Hour)

	// Cursor on session s-3 (last_seen = base + 3h). Composite predicate
	// keeps sessions with last_seen < cursor → s-0, s-1, s-2.
	cur := &SessionCursor{Ts: base.Add(3 * time.Hour), Sid: "s-3"}
	d, err := BuildDetail(dir, hash, "", false, DetailOpts{
		MessageLimit: 200, SessionLimit: 200, CWDFilter: "/p",
		SessionBefore: cur,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, s := range d.Groups[0].Sessions {
		got = append(got, s.SessionID)
	}
	// Sorted last_seen desc → s-2 first.
	if len(got) != 3 || got[0] != "s-2" || got[2] != "s-0" {
		t.Errorf("got %v want [s-2 s-1 s-0]", got)
	}
}

func TestBuildDetail_CWDFilterNoMatch(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-a")
	writeJSONL(t, dir, "2026-05-17",
		Entry{KeyHash: hash, Timestamp: time.Now(), CWD: "/exists", SessionID: "s", Prompt: "x"},
	)
	d, err := BuildDetail(dir, hash, "", false, DetailOpts{
		SessionLimit: 200, CWDFilter: "/missing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Groups) != 0 {
		t.Errorf("expected empty groups, got %+v", d.Groups)
	}
	// Totals reflect the filtered scan (scoped to /missing → 0 of everything).
	if d.TotalMessages != 0 || d.TotalSessions != 0 || d.TotalCWDs != 0 {
		t.Errorf("filtered totals nonzero: %+v", d)
	}
}

func TestBuildDetail_HeadersOnlyEmitsEmptyArray(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-a")
	base := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	seedSessions(t, dir, "2026-05-17", hash, "/p1", 3, base, time.Minute)
	seedSessions(t, dir, "2026-05-17", hash, "/p2", 5, base.Add(time.Hour), time.Minute)

	d, err := BuildDetail(dir, hash, "", false, DetailOpts{
		SessionLimit: 200, InitialCWDs: 20, HeadersOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Groups) != 2 {
		t.Fatalf("groups=%d want 2", len(d.Groups))
	}
	for _, g := range d.Groups {
		if g.Sessions == nil {
			t.Errorf("Sessions nil for %s (must be []Session{} so JSON emits [])", g.CWD)
		}
		if len(g.Sessions) != 0 {
			t.Errorf("Sessions len=%d want 0 for %s", len(g.Sessions), g.CWD)
		}
		if g.SessionCount == 0 {
			t.Errorf("SessionCount=0 for %s — meta must survive headers_only", g.CWD)
		}
		if !g.HasMore {
			t.Errorf("HasMore=false for %s — should be true when SessionCount > 0", g.CWD)
		}
	}

	// JSON round-trip: emits `[]` not `null`.
	raw, err := json.Marshal(d.Groups[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"sessions":[]`) {
		t.Errorf("headers_only marshal lost empty-array shape: %s", raw)
	}
}

func TestBuildDetail_LazyCWDsMarshalEmptyArray(t *testing.T) {
	// Overview-mode lazy-trim (CWDs past initial_cwds) shares the
	// "empty-but-not-nil" invariant with headers_only. Without it, JSON
	// emits `null` and breaks `group.sessions.length` on the TS side.
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-a")
	base := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		seedSessions(t, dir, "2026-05-17", hash, "/c"+strconv.Itoa(i), 2,
			base.Add(time.Duration(i)*time.Hour), time.Minute)
	}
	d, err := BuildDetail(dir, hash, "", false, DetailOpts{
		SessionLimit: 200, InitialCWDs: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// groups[0] is the most recent — has sessions. groups[1..] are lazy.
	if len(d.Groups) < 2 {
		t.Fatalf("expected ≥2 groups, got %d", len(d.Groups))
	}
	raw, err := json.Marshal(d.Groups[1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"sessions":[]`) {
		t.Errorf("lazy CWD must marshal sessions as [] (got %s)", raw)
	}
}

func TestBuildDetail_CursorFiltersAllSessions(t *testing.T) {
	// Cursor older than every session → response carries an empty
	// sessions array (NOT nil), has_more=false. Frontend merge then
	// becomes a no-op which is the right behavior.
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-a")
	base := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	seedSessions(t, dir, "2026-05-17", hash, "/p", 3, base, time.Minute)

	cur := &SessionCursor{Ts: base.Add(-time.Hour), Sid: "any"}
	d, err := BuildDetail(dir, hash, "", false, DetailOpts{
		SessionLimit: 200, CWDFilter: "/p", SessionBefore: cur,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(d.Groups))
	}
	g := d.Groups[0]
	if g.Sessions == nil || len(g.Sessions) != 0 {
		t.Errorf("expected empty []Session{}, got %v", g.Sessions)
	}
	if g.HasMore {
		t.Errorf("HasMore=true with cursor older than everything")
	}
	if g.SessionCount != 3 {
		t.Errorf("SessionCount=%d want 3 (whole-CWD count, not filtered)", g.SessionCount)
	}
	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"sessions":[]`) {
		t.Errorf("cursor-empty must marshal as [] (got %s)", raw)
	}
}

func TestBuildDetail_SessionIDWithPipeInCursor(t *testing.T) {
	// Session IDs are arbitrary strings; one containing '|' must not
	// confuse the composite cursor. handlers.go uses SplitN(n=2) which
	// preserves the unsplit remainder — this test pins that invariant so
	// a switch to Split() (which would split on every '|') gets caught.
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-a")
	t0 := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	writeJSONL(t, dir, "2026-05-17",
		Entry{KeyHash: hash, Timestamp: t0, CWD: "/p", SessionID: "weird|sid|a", Prompt: "x"},
		Entry{KeyHash: hash, Timestamp: t0, CWD: "/p", SessionID: "weird|sid|b", Prompt: "x"},
	)
	// Cursor on "weird|sid|a" must let "weird|sid|b" through (sid asc).
	d, err := BuildDetail(dir, hash, "", false, DetailOpts{
		SessionLimit: 10, CWDFilter: "/p",
		SessionBefore: &SessionCursor{Ts: t0, Sid: "weird|sid|a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Groups) != 1 || len(d.Groups[0].Sessions) != 1 {
		t.Fatalf("expected 1 group with 1 session, got %+v", d.Groups)
	}
	if d.Groups[0].Sessions[0].SessionID != "weird|sid|b" {
		t.Errorf("expected weird|sid|b after cursor, got %s", d.Groups[0].Sessions[0].SessionID)
	}
}

func TestBuildDetail_MessageBeforePaging(t *testing.T) {
	// 500 messages in one session. With MessageLimit=200, the initial
	// load returns the 200 most recent (msgs 300..499). MessageBefore
	// targeting msg 300's timestamp must return msgs 100..299 (still 200
	// of the older slice) and report Truncated=true (msgs 0..99 still
	// older). Another page with cursor on msg 100 returns msgs 0..99 and
	// reports Truncated=false.
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-a")
	base := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	entries := make([]Entry, 500)
	for i := 0; i < 500; i++ {
		entries[i] = Entry{
			KeyHash:   hash,
			Timestamp: base.Add(time.Duration(i) * time.Second),
			CWD:       "/p",
			SessionID: "s",
			Prompt:    "p",
		}
	}
	writeJSONL(t, dir, "2026-05-17", entries...)

	// Initial load: top 200 most recent, Truncated=true.
	d0, err := BuildDetail(dir, hash, "", false, DetailOpts{
		MessageLimit: 200, SessionLimit: 200, CWDFilter: "/p", SessionFilter: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	sess0 := d0.Groups[0].Sessions[0]
	if len(sess0.Messages) != 200 || sess0.MessageCount != 500 || !sess0.Truncated {
		t.Fatalf("initial page wrong: msgs=%d count=%d trunc=%v", len(sess0.Messages), sess0.MessageCount, sess0.Truncated)
	}
	if !sess0.Messages[0].Timestamp.Equal(base.Add(300 * time.Second)) {
		t.Errorf("oldest of initial page should be msg 300, got %v", sess0.Messages[0].Timestamp)
	}

	// Page 2: cursor on msg 300 → return msgs 100..299, still Truncated.
	d1, err := BuildDetail(dir, hash, "", false, DetailOpts{
		MessageLimit:  200,
		CWDFilter:     "/p",
		SessionFilter: "s",
		MessageBefore: base.Add(300 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	sess1 := d1.Groups[0].Sessions[0]
	if len(sess1.Messages) != 200 || sess1.MessageCount != 500 || !sess1.Truncated {
		t.Fatalf("page2 wrong: msgs=%d count=%d trunc=%v", len(sess1.Messages), sess1.MessageCount, sess1.Truncated)
	}
	if !sess1.Messages[0].Timestamp.Equal(base.Add(100 * time.Second)) {
		t.Errorf("oldest of page2 should be msg 100, got %v", sess1.Messages[0].Timestamp)
	}
	if !sess1.Messages[199].Timestamp.Equal(base.Add(299 * time.Second)) {
		t.Errorf("newest of page2 should be msg 299, got %v", sess1.Messages[199].Timestamp)
	}

	// Page 3: cursor on msg 100 → return msgs 0..99, NOT Truncated.
	d2, err := BuildDetail(dir, hash, "", false, DetailOpts{
		MessageLimit:  200,
		CWDFilter:     "/p",
		SessionFilter: "s",
		MessageBefore: base.Add(100 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	sess2 := d2.Groups[0].Sessions[0]
	if len(sess2.Messages) != 100 || sess2.Truncated {
		t.Fatalf("page3 wrong: msgs=%d trunc=%v (want 100/false)", len(sess2.Messages), sess2.Truncated)
	}
}

func TestBuildDetail_MessageBeforeExactBoundaryTiedTs(t *testing.T) {
	// Edge case: two messages share the exact timestamp that the cursor
	// lands on. MessageBefore uses strict `Before()` (exclusive), so both
	// tied messages at cursor.ts are excluded — documented limitation.
	// Behavior verified: with MessageLimit=2 and 4 msgs (msg0@t0, two
	// tied @t1, msg3@t2), cursor on t1 returns msg0 only (1 msg, both t1
	// excluded). eligibleCount=1 < limit=2 → Truncated=false.
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-a")
	t0 := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Second)
	t2 := t0.Add(2 * time.Second)
	writeJSONL(t, dir, "2026-05-17",
		Entry{KeyHash: hash, Timestamp: t0, CWD: "/p", SessionID: "s", Prompt: "msg0"},
		Entry{KeyHash: hash, Timestamp: t1, CWD: "/p", SessionID: "s", Prompt: "msg1a", Role: "user"},
		Entry{KeyHash: hash, Timestamp: t1, CWD: "/p", SessionID: "s", Prompt: "msg1b", Role: "assistant"},
		Entry{KeyHash: hash, Timestamp: t2, CWD: "/p", SessionID: "s", Prompt: "msg2"},
	)

	d, err := BuildDetail(dir, hash, "", false, DetailOpts{
		MessageLimit:  2,
		CWDFilter:     "/p",
		SessionFilter: "s",
		MessageBefore: t1, // strict-less-than → excludes both tied @t1
	})
	if err != nil {
		t.Fatal(err)
	}
	sess := d.Groups[0].Sessions[0]
	if len(sess.Messages) != 1 {
		t.Fatalf("expected 1 msg (msg0 only), got %d", len(sess.Messages))
	}
	if !sess.Messages[0].Timestamp.Equal(t0) {
		t.Errorf("expected msg0@t0, got %v", sess.Messages[0].Timestamp)
	}
	if sess.Truncated {
		t.Error("Truncated=true but only 1 msg passed filter and limit was 2")
	}
}

func TestBuildDetail_SessionFilterScopesResponse(t *testing.T) {
	// Two sessions in one CWD. SessionFilter returns just one of them in
	// Sessions, but SessionCount on the CWDGroup reflects both — so the
	// UI can still display "1 of 2 sessions".
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-a")
	base := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	writeJSONL(t, dir, "2026-05-17",
		Entry{KeyHash: hash, Timestamp: base, CWD: "/p", SessionID: "s-keep", Prompt: "x"},
		Entry{KeyHash: hash, Timestamp: base.Add(time.Hour), CWD: "/p", SessionID: "s-drop", Prompt: "x"},
	)
	d, err := BuildDetail(dir, hash, "", false, DetailOpts{
		MessageLimit: 200, CWDFilter: "/p", SessionFilter: "s-keep",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Groups) != 1 {
		t.Fatalf("groups=%d want 1", len(d.Groups))
	}
	g := d.Groups[0]
	if g.SessionCount != 2 {
		t.Errorf("SessionCount=%d want 2 (full CWD count)", g.SessionCount)
	}
	if len(g.Sessions) != 1 || g.Sessions[0].SessionID != "s-keep" {
		t.Errorf("expected only s-keep in Sessions, got %+v", g.Sessions)
	}
}

func TestBuildDetail_InitialCWDsLazyTrim(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-a")
	base := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	// Five CWDs with distinct last_seen so sort order is deterministic.
	for i := 0; i < 5; i++ {
		seedSessions(t, dir, "2026-05-17", hash, "/c"+strconv.Itoa(i), 2,
			base.Add(time.Duration(i)*time.Hour), time.Minute)
	}
	d, err := BuildDetail(dir, hash, "", false, DetailOpts{
		SessionLimit: 200, InitialCWDs: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Groups) != 5 {
		t.Fatalf("groups=%d want 5", len(d.Groups))
	}
	for i, g := range d.Groups {
		if i < 3 {
			if len(g.Sessions) != 2 {
				t.Errorf("group[%d] (%s) len(Sessions)=%d want 2 (inlined)", i, g.CWD, len(g.Sessions))
			}
		} else {
			if g.Sessions == nil || len(g.Sessions) != 0 {
				t.Errorf("group[%d] (%s) should be lazy ([]), got %v", i, g.CWD, g.Sessions)
			}
			if !g.HasMore {
				t.Errorf("group[%d] (%s) HasMore=false on lazy CWD", i, g.CWD)
			}
			if g.SessionCount != 2 {
				t.Errorf("group[%d] (%s) SessionCount=%d want 2", i, g.CWD, g.SessionCount)
			}
		}
	}
}

// Claude Code subagent shares the parent's SessionID and carries an AgentID;
// the reader's second pass must inherit the parent's CWD so the subagent
// row lands in the same session card as the dispatching turn — not in a
// separate "(unknown)" group.
func TestBuildDetail_LinksClaudeCodeSubagentToParentCWD(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-alice")
	t0 := time.Date(2026, 5, 20, 10, 25, 27, 0, time.UTC)

	writeJSONL(t, dir, "2026-05-20",
		Entry{KeyHash: hash, Timestamp: t0, CWD: "/Users/u/proj", SessionID: "sid-parent", Client: ClientClaudeCode, Model: "gpt-5.4-mini", Role: "user", Prompt: "spawn subagent tính 1+1"},
		Entry{KeyHash: hash, Timestamp: t0.Add(time.Second), SessionID: "sid-parent", AgentID: "a84564f0326e0281b", Client: ClientClaudeCode, Model: "haiku-4-5", Role: "user", Prompt: "Calculate 1+1"},
		Entry{KeyHash: hash, Timestamp: t0.Add(2 * time.Second), SessionID: "sid-parent", AgentID: "a84564f0326e0281b", Client: ClientClaudeCode, Model: "haiku-4-5", Role: "assistant", Prompt: "2"},
	)

	d, err := BuildDetail(dir, hash, "sk-a...lice", true, DetailOpts{MessageLimit: 200, SessionLimit: 200, InitialCWDs: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Groups) != 1 || d.Groups[0].CWD != "/Users/u/proj" {
		t.Fatalf("expected single group /Users/u/proj, got %+v", d.Groups)
	}
	if len(d.Groups[0].Sessions) != 1 || d.Groups[0].Sessions[0].SessionID != "sid-parent" {
		t.Fatalf("expected single session sid-parent, got %+v", d.Groups[0].Sessions)
	}
	msgs := d.Groups[0].Sessions[0].Messages
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages in parent session, got %d", len(msgs))
	}
	// First is parent prompt, second is subagent user, third is subagent assistant.
	if msgs[0].IsSubagent {
		t.Errorf("parent message marked as subagent")
	}
	for i := 1; i < 3; i++ {
		if !msgs[i].IsSubagent {
			t.Errorf("msg[%d] missing IsSubagent flag", i)
		}
		if msgs[i].SubagentID != "a84564f0" {
			t.Errorf("msg[%d] SubagentID=%q want a84564f0", i, msgs[i].SubagentID)
		}
	}
}

// Opencode subagent has its OWN SessionID different from the parent. The
// reader merges its messages into the parent's session card using the
// ParentSessionID pointer so the conversation reads as one thread.
func TestBuildDetail_MergesOpencodeSubagentIntoParentSession(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-alice")
	t0 := time.Date(2026, 5, 20, 10, 24, 16, 0, time.UTC)

	writeJSONL(t, dir, "2026-05-20",
		Entry{KeyHash: hash, Timestamp: t0, CWD: "/Users/u/proj", SessionID: "ses_AAA", Client: ClientOpencode, Role: "user", Prompt: "spawn subagent tính 1+1"},
		Entry{KeyHash: hash, Timestamp: t0.Add(5 * time.Second), SessionID: "ses_BBB", ParentSessionID: "ses_AAA", Client: ClientOpencode, Role: "user", Prompt: "Calculate 1+1"},
	)

	d, err := BuildDetail(dir, hash, "sk-a...lice", true, DetailOpts{MessageLimit: 200, SessionLimit: 200, InitialCWDs: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d: %+v", len(d.Groups), d.Groups)
	}
	if len(d.Groups[0].Sessions) != 1 || d.Groups[0].Sessions[0].SessionID != "ses_AAA" {
		t.Fatalf("expected merge into ses_AAA, got %+v", d.Groups[0].Sessions)
	}
	msgs := d.Groups[0].Sessions[0].Messages
	if len(msgs) != 2 {
		t.Fatalf("expected 2 merged messages, got %d", len(msgs))
	}
	if msgs[1].IsSubagent != true {
		t.Errorf("subagent message not flagged: %+v", msgs[1])
	}
	// SubagentID is the tail of the subagent's own session id.
	if msgs[1].SubagentID == "" {
		t.Errorf("SubagentID empty on opencode subagent")
	}
}

// Orphan subagent (parent rolled out of retention or never present) is
// dropped at render time. Standalone dispatcher framing ("Spawn a subagent
// to…", "CRITICAL: Respond with TEXT ONLY…") carries no human context, so
// surfacing it as a separate session card is pure noise. Disk content is
// untouched — a later scan with the parent back in the window will render
// the subagent normally.
func TestBuildDetail_OrphanSubagentDropped(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-alice")
	t0 := time.Date(2026, 5, 20, 10, 30, 0, 0, time.UTC)

	// One real parent turn under /Users/u/proj so the response has a real
	// group to assert against. Then two orphans that must NOT appear.
	writeJSONL(t, dir, "2026-05-20",
		Entry{KeyHash: hash, Timestamp: t0, CWD: "/Users/u/proj", SessionID: "sid-real", Client: ClientClaudeCode, Role: "user", Prompt: "real user message"},
		// Claude Code orphan: shares "sid-missing-parent" with nothing.
		Entry{KeyHash: hash, Timestamp: t0.Add(time.Second), SessionID: "sid-missing-parent", AgentID: "agentX", Client: ClientClaudeCode, Role: "user", Prompt: "orphan cc"},
		// Opencode orphan: points at "ses_NOPE" which never existed.
		Entry{KeyHash: hash, Timestamp: t0.Add(2 * time.Second), SessionID: "ses_BBB", ParentSessionID: "ses_NOPE", Client: ClientOpencode, Role: "user", Prompt: "orphan oc"},
	)

	d, err := BuildDetail(dir, hash, "sk-a...lice", true, DetailOpts{MessageLimit: 200, SessionLimit: 200, InitialCWDs: 20})
	if err != nil {
		t.Fatal(err)
	}
	// Only the real parent's group must surface — no "(unknown)" card.
	if len(d.Groups) != 1 || d.Groups[0].CWD != "/Users/u/proj" {
		t.Fatalf("expected 1 /Users/u/proj group, got %+v", d.Groups)
	}
	if len(d.Groups[0].Sessions) != 1 || d.Groups[0].Sessions[0].SessionID != "sid-real" {
		t.Fatalf("expected only sid-real session, got %+v", d.Groups[0].Sessions)
	}
	// Totals reflect the dropped entries (1 message, 1 session, 1 CWD), not
	// the raw 3 entries written to disk.
	if d.TotalMessages != 1 {
		t.Errorf("TotalMessages=%d want 1 (orphans excluded)", d.TotalMessages)
	}
	if d.TotalSessions != 1 {
		t.Errorf("TotalSessions=%d want 1 (orphans excluded)", d.TotalSessions)
	}
	if d.TotalCWDs != 1 {
		t.Errorf("TotalCWDs=%d want 1 ((unknown) bucket suppressed)", d.TotalCWDs)
	}
}

// Claude Code wraps slash commands in a chained <command-name>/<command-message>/
// <command-args> preamble. extract.go's per-tag isWrapperOnly cannot match
// the multi-tag chain, so the wrapper lands on disk as the first ~130 bytes
// of the prompt and visually drowns the actual user question. Reader strips
// it at render time so historical entries also display clean.
func TestBuildDetail_StripsCommandWrapperPrefix(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-alice")
	t0 := time.Date(2026, 5, 20, 11, 0, 0, 0, time.UTC)

	// Three flavors observed in real logs:
	//   1. /clear + user question — preamble + double newline + text
	//   2. /model with args — same shape, args tag non-empty
	//   3. plain text containing the literal "<command-name>" later in
	//      the body — must NOT be stripped (not at the start)
	writeJSONL(t, dir, "2026-05-20",
		Entry{
			KeyHash: hash, Timestamp: t0, CWD: "/p", SessionID: "s1",
			Client: ClientClaudeCode, Role: "user",
			Prompt: "<command-name>/clear</command-name>\n            <command-message>clear</command-message>\n            <command-args></command-args>\n\n\nreal question here",
		},
		Entry{
			KeyHash: hash, Timestamp: t0.Add(time.Second), CWD: "/p", SessionID: "s1",
			Client: ClientClaudeCode, Role: "user",
			Prompt: "<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args>claude-opus-4-7[1M]</command-args>\n\nwhat does H2 defer mean?",
		},
		Entry{
			KeyHash: hash, Timestamp: t0.Add(2 * time.Second), CWD: "/p", SessionID: "s1",
			Client: ClientClaudeCode, Role: "user",
			Prompt: "look at this XML: <command-name>foo</command-name> — what is it?",
		},
	)

	d, err := BuildDetail(dir, hash, "sk-a...lice", true, DetailOpts{MessageLimit: 200, SessionLimit: 200, InitialCWDs: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Groups) != 1 || len(d.Groups[0].Sessions) != 1 {
		t.Fatalf("expected 1 group / 1 session, got %+v", d.Groups)
	}
	msgs := d.Groups[0].Sessions[0].Messages
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	if msgs[0].Prompt != "real question here" {
		t.Errorf("msg[0].Prompt=%q want %q", msgs[0].Prompt, "real question here")
	}
	if msgs[1].Prompt != "what does H2 defer mean?" {
		t.Errorf("msg[1].Prompt=%q want %q", msgs[1].Prompt, "what does H2 defer mean?")
	}
	if msgs[2].Prompt != "look at this XML: <command-name>foo</command-name> — what is it?" {
		t.Errorf("msg[2].Prompt=%q (must not strip mid-prompt wrapper)", msgs[2].Prompt)
	}
}

// Unit coverage for stripCommandWrapperPrefix edge cases that the
// integration test above doesn't isolate cleanly. Pin behavior so future
// edits to the helper don't silently degrade preamble stripping or start
// mangling legitimate content.
func TestStripCommandWrapperPrefix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain text", "hello world", "hello world"},
		{"single command-name + body",
			"<command-name>/clear</command-name>\n\nhi",
			"hi"},
		{"chained name+message+args + body",
			"<command-name>/clear</command-name>\n<command-message>clear</command-message>\n<command-args></command-args>\n\nreal text",
			"real text"},
		{"chained, empty body — preamble only",
			"<command-name>/clear</command-name>\n<command-message>clear</command-message>\n<command-args></command-args>",
			""},
		{"leading whitespace before preamble",
			"   \n<command-name>/clear</command-name>\nbody",
			"body"},
		{"unknown command-X tag — bail at first non-wrapper tag",
			"<command-foo>bar</command-foo>real",
			"<command-foo>bar</command-foo>real"},
		{"wrapper mid-prompt is preserved",
			"hello <command-name>/clear</command-name>",
			"hello <command-name>/clear</command-name>"},
		{"malformed (open tag, no close) — return original",
			"<command-name>/clear\nhi",
			"<command-name>/clear\nhi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripCommandWrapperPrefix(tc.in)
			if got != tc.want {
				t.Errorf("stripCommandWrapperPrefix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Regression test for the multi-turn case the oracle flagged: a single
// subagent dispatch produces dozens of turns (28 verified in production
// logs), all sharing the same AgentID + parent SessionID. They must all
// land in the parent session, not 28 separate "(unknown)" cards.
func TestBuildDetail_LinksMultiTurnSubagentBatch(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-alice")
	t0 := time.Date(2026, 5, 19, 12, 56, 0, 0, time.UTC)

	es := []Entry{
		{KeyHash: hash, Timestamp: t0, CWD: "/Users/u/proj", SessionID: "sid-parent", Client: ClientClaudeCode, Role: "user", Prompt: "Spawn a deep Explore"},
	}
	for i := 1; i <= 28; i++ {
		es = append(es, Entry{
			KeyHash:   hash,
			Timestamp: t0.Add(time.Duration(i) * time.Second),
			SessionID: "sid-parent",
			AgentID:   "af45b4596cffacef7",
			Client:    ClientClaudeCode,
			Role:      "user",
			Prompt:    "subagent turn " + strconv.Itoa(i),
		})
	}
	writeJSONL(t, dir, "2026-05-19", es...)

	d, err := BuildDetail(dir, hash, "sk-a...lice", true, DetailOpts{MessageLimit: 500, SessionLimit: 200, InitialCWDs: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Groups) != 1 || d.Groups[0].CWD != "/Users/u/proj" {
		t.Fatalf("expected single group /Users/u/proj, got %+v", d.Groups)
	}
	if len(d.Groups[0].Sessions) != 1 {
		t.Fatalf("expected 28 turns to land in ONE session, got %d separate cards", len(d.Groups[0].Sessions))
	}
	if got := d.Groups[0].Sessions[0].MessageCount; got != 29 {
		t.Errorf("MessageCount=%d want 29 (1 parent + 28 subagent)", got)
	}
	subFlagged := 0
	for _, m := range d.Groups[0].Sessions[0].Messages {
		if m.IsSubagent {
			subFlagged++
		}
	}
	if subFlagged != 28 {
		t.Errorf("IsSubagent count=%d want 28", subFlagged)
	}
}

func TestSearchMessages_EmptyDir(t *testing.T) {
	res, err := SearchMessages(t.TempDir(), "abc123", "hello", 50)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || len(res.Matches) != 0 || res.TotalMatches != 0 || res.Truncated {
		t.Errorf("expected empty result, got %+v", res)
	}
}

func TestSearchMessages_EmptyQuery(t *testing.T) {
	// Defensive: caller should validate but the helper must not match
	// every message when q is blank.
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-alice")
	writeJSONL(t, dir, "2026-05-15",
		Entry{KeyHash: hash, Timestamp: time.Now(), SessionID: "s1", CWD: "/proj", Prompt: "hello world"},
	)
	res, err := SearchMessages(dir, hash, "   ", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 0 {
		t.Errorf("blank query should match nothing, got %d hits", len(res.Matches))
	}
}

func TestSearchMessages_BasicMatch(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-alice")
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	writeJSONL(t, dir, "2026-05-15",
		Entry{KeyHash: hash, Timestamp: now, SessionID: "s1", CWD: "/proj", Prompt: "fix the auth bug"},
		Entry{KeyHash: hash, Timestamp: now.Add(time.Hour), SessionID: "s1", CWD: "/proj", Prompt: "update README only"},
		Entry{KeyHash: hash, Timestamp: now.Add(2 * time.Hour), SessionID: "s2", CWD: "/proj", Prompt: "AUTH MIDDLEWARE refactor"},
	)
	res, err := SearchMessages(dir, hash, "auth", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 2 || res.TotalMatches != 2 || res.Truncated {
		t.Fatalf("expected 2 matches, got %+v", res)
	}
	// Desc by ts: AUTH MIDDLEWARE first.
	if !strings.Contains(strings.ToLower(res.Matches[0].Excerpt), "auth middleware") {
		t.Errorf("matches[0] excerpt=%q (want newest first)", res.Matches[0].Excerpt)
	}
	if res.Matches[0].SessionID != "s2" {
		t.Errorf("matches[0] session=%q want s2", res.Matches[0].SessionID)
	}
}

func TestSearchMessages_OtherKeyIgnored(t *testing.T) {
	dir := t.TempDir()
	alice := ratelimit.HashKey("sk-alice")
	bob := ratelimit.HashKey("sk-bob")
	now := time.Now()
	writeJSONL(t, dir, "2026-05-15",
		Entry{KeyHash: alice, Timestamp: now, SessionID: "a", CWD: "/proj", Prompt: "auth"},
		Entry{KeyHash: bob, Timestamp: now, SessionID: "b", CWD: "/proj", Prompt: "auth"},
	)
	res, _ := SearchMessages(dir, alice, "auth", 50)
	if len(res.Matches) != 1 || res.Matches[0].SessionID != "a" {
		t.Errorf("cross-key leak: %+v", res.Matches)
	}
}

func TestSearchMessages_ExcerptClipping(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-alice")
	long := strings.Repeat("x ", 100) + "PATTERN" + strings.Repeat(" y", 100)
	writeJSONL(t, dir, "2026-05-15",
		Entry{KeyHash: hash, Timestamp: time.Now(), SessionID: "s", CWD: "/p", Prompt: long},
	)
	res, _ := SearchMessages(dir, hash, "pattern", 10)
	if len(res.Matches) != 1 {
		t.Fatalf("got %d matches", len(res.Matches))
	}
	ex := res.Matches[0].Excerpt
	if !strings.Contains(strings.ToLower(ex), "pattern") {
		t.Errorf("excerpt missing match: %q", ex)
	}
	// Should be clipped on both sides.
	if !strings.HasPrefix(ex, "…") || !strings.HasSuffix(ex, "…") {
		t.Errorf("excerpt should be clipped both sides, got %q", ex)
	}
	// And much shorter than the raw prompt (60×2 window + match + ellipses ≈ 130 chars).
	if len(ex) > 200 {
		t.Errorf("excerpt too long: %d chars (%q)", len(ex), ex)
	}
}

func TestSearchMessages_WhitespaceNormalized(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-alice")
	writeJSONL(t, dir, "2026-05-15",
		Entry{KeyHash: hash, Timestamp: time.Now(), SessionID: "s", CWD: "/p",
			Prompt: "line one\nline\twith\rtabs\n\n\nfind here\n   trailing"},
	)
	res, _ := SearchMessages(dir, hash, "find", 10)
	if len(res.Matches) != 1 {
		t.Fatalf("got %d hits", len(res.Matches))
	}
	ex := res.Matches[0].Excerpt
	if strings.ContainsAny(ex, "\n\r\t") {
		t.Errorf("excerpt still has whitespace control chars: %q", ex)
	}
	// No double spaces.
	if strings.Contains(ex, "  ") {
		t.Errorf("excerpt has collapsed whitespace runs: %q", ex)
	}
}

func TestSearchMessages_LimitAndTruncated(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-alice")
	base := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	entries := make([]Entry, 0, 25)
	for i := 0; i < 25; i++ {
		entries = append(entries, Entry{
			KeyHash:   hash,
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			SessionID: "s" + strconv.Itoa(i),
			CWD:       "/p",
			Prompt:    "needle " + strconv.Itoa(i),
		})
	}
	writeJSONL(t, dir, "2026-05-15", entries...)

	res, _ := SearchMessages(dir, hash, "needle", 10)
	if len(res.Matches) != 10 || res.TotalMatches != 25 || !res.Truncated {
		t.Errorf("limit cap wrong: matches=%d total=%d trunc=%v", len(res.Matches), res.TotalMatches, res.Truncated)
	}
	// Desc by ts: newest is entry 24.
	if !strings.Contains(res.Matches[0].Excerpt, "needle 24") {
		t.Errorf("matches[0]=%q (want newest 'needle 24')", res.Matches[0].Excerpt)
	}
}

func TestSearchMessages_LimitDefaultsAndCap(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-alice")
	writeJSONL(t, dir, "2026-05-15",
		Entry{KeyHash: hash, Timestamp: time.Now(), SessionID: "s", CWD: "/p", Prompt: "hello"},
	)
	// limit ≤ 0 → default 200 (well above 1 match here, no truncation).
	if res, _ := SearchMessages(dir, hash, "hello", 0); res.Truncated {
		t.Errorf("default limit should not truncate single match: %+v", res)
	}
	if res, _ := SearchMessages(dir, hash, "hello", -5); res.Truncated {
		t.Errorf("negative limit should not truncate single match: %+v", res)
	}
	// limit > 500 should still scan but not blow past internal cap. The
	// public contract is "we cap at 500", verified by the API handler test;
	// here we just check it doesn't crash on a huge value.
	if _, err := SearchMessages(dir, hash, "hello", 100_000); err != nil {
		t.Errorf("large limit errored: %v", err)
	}
}

func TestSearchMessages_SubagentBucketing(t *testing.T) {
	// Claude Code subagent: shares parent SessionID, has AgentID, no CWD.
	// A search hit on the subagent's prompt should report the PARENT's
	// (cwd, session_id) so the UI deep-links into the parent's reading pane.
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-alice")
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	writeJSONL(t, dir, "2026-05-15",
		// Parent turn (defines the (cwd, sid) bucket).
		Entry{KeyHash: hash, Timestamp: now, SessionID: "main", CWD: "/work", Prompt: "kick off"},
		// Subagent turn (no own CWD; AgentID present).
		Entry{KeyHash: hash, Timestamp: now.Add(time.Minute), SessionID: "main", AgentID: "agent-deadbeef", Prompt: "find unicorn references"},
	)
	res, _ := SearchMessages(dir, hash, "unicorn", 50)
	if len(res.Matches) != 1 {
		t.Fatalf("got %d hits", len(res.Matches))
	}
	hit := res.Matches[0]
	if hit.CWD != "/work" {
		t.Errorf("CWD bucketed to %q want parent /work", hit.CWD)
	}
	if hit.SessionID != "main" {
		t.Errorf("SessionID=%q want parent main", hit.SessionID)
	}
	if !hit.IsSubagent || hit.SubagentID == "" {
		t.Errorf("subagent flags missing: %+v", hit)
	}
}

func TestSearchMessages_OrphanSubagentDropped(t *testing.T) {
	// Subagent with no findable parent: BuildDetail drops it to avoid
	// surfacing dispatcher framing with no human context. Search must
	// behave the same so results are consistent with the tree.
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-alice")
	writeJSONL(t, dir, "2026-05-15",
		Entry{KeyHash: hash, Timestamp: time.Now(), SessionID: "lonely", AgentID: "agent-x",
			Prompt: "needle dispatch context only"},
	)
	res, _ := SearchMessages(dir, hash, "needle", 50)
	if len(res.Matches) != 0 {
		t.Errorf("orphan subagent should be dropped, got %d hits", len(res.Matches))
	}
}

func TestSearchMessages_UnicodeAndCaseFolding(t *testing.T) {
	dir := t.TempDir()
	hash := ratelimit.HashKey("sk-alice")
	writeJSONL(t, dir, "2026-05-15",
		// Vietnamese diacritics: lowercase query should still match.
		Entry{KeyHash: hash, Timestamp: time.Now(), SessionID: "s1", CWD: "/p",
			Prompt: "Cập nhật lại giao diện prompts"},
		// Mixed-case English.
		Entry{KeyHash: hash, Timestamp: time.Now().Add(time.Minute), SessionID: "s2", CWD: "/p",
			Prompt: "REFACTOR the Reader"},
	)
	// "GIAO" (uppercased ASCII portion of Vietnamese) should match "giao".
	if res, _ := SearchMessages(dir, hash, "GIAO", 50); len(res.Matches) != 1 {
		t.Errorf("uppercase ASCII query should case-fold: %+v", res)
	}
	// Vietnamese diacritic query.
	if res, _ := SearchMessages(dir, hash, "giao diện", 50); len(res.Matches) != 1 {
		t.Errorf("vietnamese query should match: %+v", res)
	}
	if res, _ := SearchMessages(dir, hash, "refactor", 50); len(res.Matches) != 1 {
		t.Errorf("english case-fold: %+v", res)
	}
}
