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
