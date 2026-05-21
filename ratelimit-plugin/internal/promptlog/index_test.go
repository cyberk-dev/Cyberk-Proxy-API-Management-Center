package promptlog

import (
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cyberk/ratelimit-plugin/internal/ratelimit"
)

// TestIndex_ColdStart_Equivalence is the go/no-go gate for the in-memory
// index path. For each representative fixture, both code paths
// (scanAll-backed top-level functions and Index methods on a freshly-built
// NewIndex(dir)) must produce reflect.DeepEqual output. Failures here mean
// the index diverges from the disk-scan baseline — block the rollout.
//
// Coverage chosen to flush out the failure modes called out in the
// implementation plan and oracle review:
//   - subagent linking (Claude Code shared-SessionID and opencode
//     ParentSessionID), including ORPHAN drops
//   - multi-day file ordering (parent in older file, subagent in newer)
//   - sliding-window pagination (MessageLimit, SessionLimit, InitialCWDs)
//   - cursor + cwd filter combos (HeadersOnly, CWDFilter, SessionFilter,
//     SessionBefore, MessageBefore)
//   - ts-tied sessions (composite cursor determinism)
//   - search hits + truncation
func TestIndex_ColdStart_Equivalence(t *testing.T) {
	dir := t.TempDir()
	alice := ratelimit.HashKey("sk-alice")
	bob := ratelimit.HashKey("sk-bob")
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)

	// File A (older): alice has 2 normal turns in /proj-a/s1, and the parent
	// session for the opencode subagent that lands tomorrow.
	writeJSONL(t, dir, "2026-05-15",
		Entry{KeyHash: alice, Timestamp: now, SessionID: "s1", CWD: "/proj-a", Client: "claude_code", Model: "claude-opus", Provider: "anthropic", Prompt: "hello alice"},
		Entry{KeyHash: alice, Timestamp: now.Add(5 * time.Minute), SessionID: "s1", CWD: "/proj-a", Client: "claude_code", Model: "claude-opus", Provider: "anthropic", Role: "assistant", Prompt: "[reply]"},
		Entry{KeyHash: alice, Timestamp: now.Add(10 * time.Minute), SessionID: "parent-oc", CWD: "/proj-oc", Client: "opencode", Model: "claude-haiku", Provider: "anthropic", Prompt: "spawn the subagent please"},
		Entry{KeyHash: bob, Timestamp: now.Add(time.Hour), SessionID: "b1", CWD: "/proj-b", Client: "claude_code", Prompt: "bob first"},
	)
	// File B (newer): alice gets the subagent turn linked to yesterday's
	// parent (cross-day ordering — exercises the sorted-merge invariant from
	// the index cold-start). Also a Claude Code subagent under s1 and an
	// orphan opencode subagent that must be DROPPED.
	writeJSONL(t, dir, "2026-05-16",
		Entry{KeyHash: alice, Timestamp: now.Add(24 * time.Hour), SessionID: "s1", AgentID: "agentABCDEF1234567", CWD: "", Client: "claude_code", Model: "claude-opus", Prompt: "subagent under s1: research X"},
		Entry{KeyHash: alice, Timestamp: now.Add(25 * time.Hour), SessionID: "ses_subaccent", ParentSessionID: "parent-oc", CWD: "", Client: "opencode", Model: "claude-haiku", Prompt: "doing the spawn work"},
		Entry{KeyHash: alice, Timestamp: now.Add(26 * time.Hour), SessionID: "ses_orphan", ParentSessionID: "parent-vanished", CWD: "", Client: "opencode", Prompt: "this parent rolled out of retention"},
		Entry{KeyHash: alice, Timestamp: now.Add(27 * time.Hour), SessionID: "s2", CWD: "/proj-a", Client: "claude_code", Prompt: "later same project"},
		// Edge cases per oracle review items #8 and #9:
		// - empty CWD on a non-subagent entry (buckets into "(unknown)" group)
		// - empty Prompt (SearchMessages must skip this entry; BuildDetail must keep it)
		Entry{KeyHash: alice, Timestamp: now.Add(28 * time.Hour), SessionID: "s-nocwd", CWD: "", Client: "claude_code", Prompt: "no cwd but has content"},
		Entry{KeyHash: alice, Timestamp: now.Add(29 * time.Hour), SessionID: "s2", CWD: "/proj-a", Client: "claude_code", Prompt: ""},
	)
	// File C: two sessions with EXACTLY the same LastSeen across different
	// CWDs — exercises the composite cursor tie-break in aggregateDetail
	// (reader.go: `s.LastSeen.Equal(cur.Ts) && s.SessionID > cur.Sid`).
	writeJSONL(t, dir, "2026-05-17",
		Entry{KeyHash: alice, Timestamp: now.Add(48 * time.Hour), SessionID: "tied-a", CWD: "/proj-tied", Client: "claude_code", Prompt: "tied a"},
		Entry{KeyHash: alice, Timestamp: now.Add(48 * time.Hour), SessionID: "tied-b", CWD: "/proj-tied", Client: "claude_code", Prompt: "tied b"},
	)

	idx, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}

	configured := []string{"sk-alice", "sk-bob"}

	// 1. ListUsers equivalence.
	wantUsers, err := ListUsers(dir, configured)
	if err != nil {
		t.Fatalf("ListUsers(scan): %v", err)
	}
	gotUsers := idx.ListUsers(configured)
	if !reflect.DeepEqual(wantUsers, gotUsers) {
		t.Errorf("ListUsers mismatch:\n want=%+v\n got=%+v", wantUsers, gotUsers)
	}

	// 2. BuildDetail across the option matrix.
	detailCases := []struct {
		name string
		key  string
		hint string
		conf bool
		opts DetailOpts
	}{
		{"alice overview default", alice, "sk-a...lice", true, DetailOpts{}},
		{"alice tight message limit", alice, "sk-a...lice", true, DetailOpts{MessageLimit: 2, SessionLimit: 200, InitialCWDs: 20}},
		{"alice CWD filter /proj-a", alice, "sk-a...lice", true, DetailOpts{MessageLimit: 200, SessionLimit: 200, CWDFilter: "/proj-a"}},
		{"alice headers_only", alice, "sk-a...lice", true, DetailOpts{HeadersOnly: true, MessageLimit: 200, SessionLimit: 200, InitialCWDs: 20}},
		{"alice session_filter s1", alice, "sk-a...lice", true, DetailOpts{MessageLimit: 200, SessionLimit: 200, CWDFilter: "/proj-a", SessionFilter: "s1"}},
		{"alice initial_cwds=1", alice, "sk-a...lice", true, DetailOpts{MessageLimit: 200, SessionLimit: 200, InitialCWDs: 1}},
		{"alice session_before in /proj-a", alice, "sk-a...lice", true, DetailOpts{
			MessageLimit: 200, SessionLimit: 200,
			CWDFilter:     "/proj-a",
			SessionBefore: &SessionCursor{Ts: now.Add(30 * time.Hour), Sid: "z"},
		}},
		// Composite-cursor tie-break: two tied-* sessions share LastSeen
		// exactly. With cursor.Sid = "tied-a", only sessions where
		// LastSeen<cursor.Ts OR (LastSeen==cursor.Ts AND SessionID>"tied-a")
		// survive — so tied-b passes, tied-a doesn't.
		{"alice tied-session cursor", alice, "sk-a...lice", true, DetailOpts{
			MessageLimit:  200,
			SessionLimit:  200,
			CWDFilter:     "/proj-tied",
			SessionBefore: &SessionCursor{Ts: now.Add(48 * time.Hour), Sid: "tied-a"},
		}},
		// Empty-CWD bucket: "(unknown)" group must aggregate correctly
		// through both paths.
		{"alice unknown CWD filter", alice, "sk-a...lice", true, DetailOpts{
			MessageLimit: 200,
			SessionLimit: 200,
			CWDFilter:    "(unknown)",
		}},
		{"alice message_before s1", alice, "sk-a...lice", true, DetailOpts{
			MessageLimit:  200,
			SessionLimit:  200,
			CWDFilter:     "/proj-a",
			SessionFilter: "s1",
			MessageBefore: now.Add(6 * time.Minute),
		}},
		{"bob default", bob, "", false, DetailOpts{}},
		{"unknown key returns empty detail", ratelimit.HashKey("sk-ghost"), "", false, DetailOpts{}},
	}
	for _, tc := range detailCases {
		t.Run("BuildDetail/"+tc.name, func(t *testing.T) {
			want, err := BuildDetail(dir, tc.key, tc.hint, tc.conf, tc.opts)
			if err != nil {
				t.Fatalf("BuildDetail(scan): %v", err)
			}
			got := idx.BuildDetail(tc.key, tc.hint, tc.conf, tc.opts)
			if !reflect.DeepEqual(want, got) {
				t.Errorf("BuildDetail mismatch:\n want=%#v\n got=%#v", want, got)
			}
		})
	}

	// 3. SearchMessages equivalence — empty hits, hit-with-subagent,
	// hit-with-truncation.
	searchCases := []struct {
		name  string
		key   string
		query string
		limit int
	}{
		{"alice hello", alice, "hello", 200},
		{"alice subagent", alice, "subagent", 200},
		{"alice no match", alice, "definitely-not-a-substring", 200},
		{"alice tight limit", alice, "alice", 1},
		// Empty-prompt entries must be skipped by SearchMessages (the
		// `if e.Prompt == ""` guard in aggregateSearch). Searching for a
		// substring of the empty-prompt session's metadata should not
		// surface it.
		{"alice search empty-prompt-session", alice, "no cwd", 200},
		{"bob match", bob, "bob", 200},
		{"unknown key empty", ratelimit.HashKey("sk-ghost"), "anything", 200},
	}
	for _, tc := range searchCases {
		t.Run("Search/"+tc.name, func(t *testing.T) {
			want, err := SearchMessages(dir, tc.key, tc.query, tc.limit)
			if err != nil {
				t.Fatalf("SearchMessages(scan): %v", err)
			}
			got := idx.SearchMessages(tc.key, tc.query, tc.limit)
			if !reflect.DeepEqual(want, got) {
				t.Errorf("Search mismatch:\n want=%#v\n got=%#v", want, got)
			}
		})
	}
}

// TestIndex_ColdStart_OrderingDeterminism specifically exercises the
// "merge in sorted filename order" invariant. The parent-session
// last-writer-wins inside aggregateDetail is order-sensitive: if a session
// id appears in two files with different CWDs, the LATER file's CWD must
// win. With files scanned in parallel and merged in arrival order, the
// outcome would be nondeterministic across boots — this test would catch
// that regression.
func TestIndex_ColdStart_OrderingDeterminism(t *testing.T) {
	dir := t.TempDir()
	alice := ratelimit.HashKey("sk-alice")
	t0 := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)

	// Same SessionID appears in both files with DIFFERENT CWDs. The later
	// file's CWD should be the one that ends up in parentBySession — and
	// therefore the one used to bucket any subagent under "shared-s1".
	writeJSONL(t, dir, "2026-05-15",
		Entry{KeyHash: alice, Timestamp: t0, SessionID: "shared-s1", CWD: "/proj-old", Client: "claude_code", Prompt: "old turn"},
	)
	writeJSONL(t, dir, "2026-05-20",
		Entry{KeyHash: alice, Timestamp: t0.Add(120 * time.Hour), SessionID: "shared-s1", CWD: "/proj-new", Client: "claude_code", Prompt: "new turn"},
		Entry{KeyHash: alice, Timestamp: t0.Add(121 * time.Hour), SessionID: "shared-s1", AgentID: "agent12345abcdef00", CWD: "", Client: "claude_code", Prompt: "subagent under shared-s1"},
	)

	// Run NewIndex many times — if merge order leaked through, we'd see
	// flakes where the subagent lands under /proj-old in some runs.
	const runs = 25
	for i := 0; i < runs; i++ {
		idx, err := NewIndex(dir)
		if err != nil {
			t.Fatalf("NewIndex iter=%d: %v", i, err)
		}
		d := idx.BuildDetail(alice, "sk-a...lice", true, DetailOpts{})
		// The subagent must be bucketed under /proj-new (the later
		// observation of shared-s1's parent CWD).
		var found bool
		for _, g := range d.Groups {
			if g.CWD == "/proj-new" {
				for _, s := range g.Sessions {
					if s.SessionID == "shared-s1" {
						for _, m := range s.Messages {
							if m.IsSubagent {
								found = true
							}
						}
					}
				}
			}
		}
		if !found {
			t.Fatalf("iter=%d: subagent under /proj-new not found; groups=%+v", i, d.Groups)
		}
	}
}

// TestWriter_AddsToIndex validates that the writer→index wiring is HOOKED
// UP — submit a happy-path entry, confirm it shows up in idx.ListUsers /
// idx.BuildDetail without anyone scanning disk. Catches the regression
// "Add was removed entirely" but does NOT prove the ordering
// (Add-after-buf.WriteByte). The ordering invariant — Add must not fire
// when the disk write fails — is enforced by TestWriter_DoesNotAddOnDiskFailure
// below and by the code comment at writer.go's Add call site.
func TestWriter_AddsToIndex(t *testing.T) {
	dir := t.TempDir()
	idx, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	w, err := NewWriter(dir, 8, nil, TemplatesConfig{}, idx)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	hash := "abc123def456"
	ts := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	w.Submit(&Entry{
		Timestamp: ts,
		KeyHash:   hash,
		SessionID: "s1",
		CWD:       "/proj",
		Client:    "claude_code",
		Model:     "claude-opus",
		Provider:  ProviderAnthropic,
		Path:      "/v1/messages",
		Prompt:    "wired through writer",
	})
	w.Close() // drains the queue, flushes file, the Add happened before WriteByte returned

	users := idx.ListUsers(nil)
	if len(users) != 1 || users[0].KeyHash != hash || users[0].MessageCount != 1 {
		t.Fatalf("writer→index wiring broken: %+v", users)
	}
	d := idx.BuildDetail(hash, "", false, DetailOpts{})
	if d.TotalMessages != 1 || len(d.Groups) != 1 || d.Groups[0].CWD != "/proj" {
		t.Fatalf("writer→index detail wrong: %+v", d)
	}
}

// TestWriter_DoesNotAddOnDiskFailure proves Add is GATED on a successful
// disk write. We sabotage the writer's daily file by pre-creating a
// directory at the path where ensureFile would `os.OpenFile(...O_WRONLY)`,
// which returns EISDIR on macOS/linux. The writer's run loop hits the
// `continue` branch at writer.go before reaching the index Add — so the
// index must stay empty.
//
// Without this test, a future refactor that moves `w.index.Add(e)` ABOVE
// the disk write would silently regress: TestWriter_AddsToIndex would
// keep passing because the happy path still observes the entry in the
// index, but production would publish phantom entries that never land on
// disk. This is exactly the ordering invariant the oracle flagged.
func TestWriter_DoesNotAddOnDiskFailure(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	// Sabotage: a directory where the daily JSONL file should land. OpenFile
	// with O_WRONLY on a directory fails immediately, ensureFile returns
	// false, the run loop's `continue` skips both write and index Add.
	sabotage := dir + "/prompts-2026-05-20.jsonl"
	if err := mkdirSabotage(sabotage); err != nil {
		t.Fatalf("sabotage setup: %v", err)
	}

	idx := &Index{byHash: make(map[string]*keyShard)}
	w, err := NewWriter(dir, 8, nil, TemplatesConfig{}, idx)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	w.Submit(&Entry{
		Timestamp: ts,
		KeyHash:   "abc",
		SessionID: "s1",
		CWD:       "/proj",
		Client:    "claude_code",
		Prompt:    "would-be-phantom",
	})
	w.Close()

	users := idx.ListUsers(nil)
	if len(users) != 0 {
		t.Fatalf("index should be empty after disk-write failure, got %+v", users)
	}
}

// mkdirSabotage creates a directory at path so that os.OpenFile(path,
// O_WRONLY|O_APPEND|O_CREATE) fails. Split out so the intent is obvious
// at the call site.
func mkdirSabotage(path string) error {
	return os.MkdirAll(path, 0o755)
}

// TestIndex_Add_LiveUpdate covers the runtime Add path. After NewIndex,
// the writer can push new entries that subsequent queries see immediately
// without re-scanning disk.
func TestIndex_Add_LiveUpdate(t *testing.T) {
	dir := t.TempDir()
	alice := ratelimit.HashKey("sk-alice")
	t0 := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	idx, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	if got := idx.ListUsers(nil); len(got) != 0 {
		t.Fatalf("empty index should produce empty user list, got %+v", got)
	}

	idx.Add(&Entry{KeyHash: alice, Timestamp: t0, SessionID: "s1", CWD: "/proj", Client: "claude_code", Prompt: "first"})
	idx.Add(&Entry{KeyHash: alice, Timestamp: t0.Add(time.Minute), SessionID: "s1", CWD: "/proj", Client: "claude_code", Role: "assistant", Prompt: "[reply]"})

	users := idx.ListUsers(nil)
	if len(users) != 1 || users[0].MessageCount != 2 || users[0].SessionCount != 1 {
		t.Fatalf("live-update users wrong: %+v", users)
	}

	d := idx.BuildDetail(alice, "", false, DetailOpts{})
	if d.TotalMessages != 2 || len(d.Groups) != 1 || len(d.Groups[0].Sessions) != 1 || len(d.Groups[0].Sessions[0].Messages) != 2 {
		t.Fatalf("live-update detail wrong: %+v", d)
	}
}

// TestIndex_ConcurrentAddAndQuery is the race-detection gate. Add+queries
// must not race when run under `go test -race`. The test deliberately
// overlaps writers with all three query methods so the detector sees them
// touching shared state.
func TestIndex_ConcurrentAddAndQuery(t *testing.T) {
	dir := t.TempDir()
	idx, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	alice := ratelimit.HashKey("sk-alice")
	t0 := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	const writes = 500
	const readers = 4
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < writes; i++ {
			idx.Add(&Entry{
				KeyHash:   alice,
				Timestamp: t0.Add(time.Duration(i) * time.Millisecond),
				SessionID: "s1",
				CWD:       "/proj",
				Client:    "claude_code",
				Prompt:    "load",
			})
		}
	}()

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < writes/4; i++ {
				_ = idx.ListUsers(nil)
				_ = idx.BuildDetail(alice, "", false, DetailOpts{})
				_ = idx.SearchMessages(alice, "load", 200)
			}
		}()
	}

	wg.Wait()

	got := idx.ListUsers(nil)
	if len(got) != 1 || got[0].MessageCount != writes {
		t.Fatalf("after concurrent writes/reads, expected %d msgs, got %+v", writes, got)
	}
}
