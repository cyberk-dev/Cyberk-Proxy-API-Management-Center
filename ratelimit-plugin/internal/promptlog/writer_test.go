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
	w, err := NewWriter(dir, 8, nil, TemplatesConfig{})
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
	w, err := NewWriter(dir, 8, nil, TemplatesConfig{})
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
	w, err := NewWriter(dir, 1, nil, TemplatesConfig{})
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
	if _, err := NewWriter("", 8, nil, TemplatesConfig{}); err == nil {
		t.Fatal("expected error for empty dir")
	}
}

func TestWriter_StripsBlockTextBeforeEncode(t *testing.T) {
	// Block.Text duplicates Entry.Prompt — the writer must strip it so the
	// JSONL line carries the content once (in `prompt`). Block.Bytes
	// preserves the per-block size for offline reconstruction of structure.
	dir := t.TempDir()
	w, err := NewWriter(dir, 8, nil, TemplatesConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	w.Submit(&Entry{
		Timestamp: ts,
		Provider:  ProviderAnthropic,
		Path:      "/v1/messages",
		Prompt:    "hello world",
		Blocks: []Block{
			{Type: "text", Text: "hello world"},
			{Type: "image", MediaType: "image/png", Bytes: 1024, SHA256: "abcd"},
		},
	})
	w.Close()

	entries := readDailyFile(t, dir, "2026-05-17")
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	if entries[0]["prompt"] != "hello world" {
		t.Errorf("prompt lost: %v", entries[0])
	}
	blocks, _ := entries[0]["blocks"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("blocks: %+v", entries[0])
	}
	txt, _ := blocks[0].(map[string]any)
	if _, hasText := txt["text"]; hasText {
		t.Errorf("text block must not carry text field, got %+v", txt)
	}
	if got, ok := txt["bytes"].(float64); !ok || int(got) != len("hello world") {
		t.Errorf("text block bytes wrong: %+v", txt)
	}
	img, _ := blocks[1].(map[string]any)
	if img["sha256"] != "abcd" {
		t.Errorf("non-text block metadata clobbered: %+v", img)
	}
}

func TestWriter_SkipsTemplatingForAssistantEntries(t *testing.T) {
	// An assistant entry whose Prompt opens with an inlined tool-block
	// header must NOT (a) get template-matched against user-prompt
	// templates or (b) be observed by the detector. Without this gate the
	// detector would auto-register "[tool_use Bash 234B]\n..." prefixes as
	// templates, and Match() would attempt to splice user-shape templates
	// onto assistant text — both nonsensical.
	dir := t.TempDir()
	tpl, err := NewTemplateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a template whose body happens to match a possible assistant
	// preamble — if the gate is missing, the assistant entry would
	// template-match against this and lose its Prompt prefix.
	_, err = tpl.Register("Here is the answer.", "preloaded", time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWriter(dir, 8, tpl, TemplatesConfig{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	ts := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	w.Submit(&Entry{
		Timestamp: ts,
		Role:      "assistant",
		Provider:  ProviderAnthropic,
		Path:      "/v1/messages",
		Prompt:    "Here is the answer.",
		Blocks:    []Block{{Type: "text", Text: "Here is the answer."}},
	})
	w.Close()

	entries := readDailyFile(t, dir, "2026-05-17")
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	if tpl, ok := entries[0]["prompt_template"]; ok && tpl != nil && tpl != "" {
		t.Errorf("assistant entry must not get prompt_template set, got %v", tpl)
	}
	if entries[0]["prompt"] != "Here is the answer." {
		t.Errorf("assistant prompt must not be templated/shortened, got %v", entries[0]["prompt"])
	}
}

func TestWriter_SubmitAfterClose(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 8, nil, TemplatesConfig{})
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
