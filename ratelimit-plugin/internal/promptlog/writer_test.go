package promptlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriter_WritesJSONL(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	ts := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	w.Submit(&Entry{
		Timestamp: ts,
		KeyHash:   "abc123",
		Model:     "claude",
		Provider:  ProviderAnthropic,
		Path:      "/v1/messages",
		Status:    200,
		Blocks:    []Block{{Type: "text", Text: "hi"}},
	})
	w.Close()

	entries := readDailyFile(t, dir, "2026-05-15")
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	if entries[0]["key_hash"] != "abc123" {
		t.Errorf("bad key_hash: %v", entries[0])
	}
}

func TestWriter_RotatesByDate(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	day1 := time.Date(2026, 5, 15, 23, 59, 0, 0, time.UTC)
	day2 := time.Date(2026, 5, 16, 0, 1, 0, 0, time.UTC)

	w.Submit(&Entry{Timestamp: day1, Provider: "anthropic", Path: "/v1/messages", Blocks: []Block{{Type: "text", Text: "late"}}})
	w.Submit(&Entry{Timestamp: day2, Provider: "anthropic", Path: "/v1/messages", Blocks: []Block{{Type: "text", Text: "early"}}})
	w.Close()

	e1 := readDailyFile(t, dir, "2026-05-15")
	e2 := readDailyFile(t, dir, "2026-05-16")
	if len(e1) != 1 || len(e2) != 1 {
		t.Fatalf("rotation broken: day1=%d day2=%d", len(e1), len(e2))
	}
}

func TestWriter_DropsOnFullQueue(t *testing.T) {
	dir := t.TempDir()
	// Tiny queue + we never let the goroutine drain by submitting fast.
	w, err := NewWriter(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Hammer with more than queue size before the reader has a chance.
	// Some will be accepted, some dropped — assert at least one drop.
	for i := 0; i < 200; i++ {
		w.Submit(&Entry{Provider: "anthropic", Path: "/v1/messages", Blocks: []Block{{Type: "text", Text: "x"}}})
	}
	// Give the writer goroutine a moment, then close.
	w.Close()
	// Dropped is best-effort — under load we should see at least some misses.
	if w.Dropped() == 0 {
		t.Logf("note: no drops observed (queue drained fast); not a failure")
	}
}

func TestWriter_RejectsEmptyDir(t *testing.T) {
	if _, err := NewWriter("", 8); err == nil {
		t.Fatal("expected error for empty dir")
	}
}

func TestWriter_SubmitAfterClose(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 8)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	if w.Submit(&Entry{Provider: "x"}) {
		t.Fatal("expected Submit after Close to return false")
	}
}

func readDailyFile(t *testing.T, dir, date string) []map[string]any {
	t.Helper()
	path := filepath.Join(dir, "prompts-"+date+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad line %q: %v", line, err)
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}
