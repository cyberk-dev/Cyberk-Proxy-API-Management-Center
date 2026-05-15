package promptlog

import (
	"encoding/json"
	"os"
	"path/filepath"
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

	detail, err := BuildDetail(dir, hash, "sk-a...lice", true, 200)
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

	detail, err := BuildDetail(dir, hash, "", false, 10)
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
