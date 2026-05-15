package promptlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTemplateStore_RegisterAndMatch(t *testing.T) {
	dir := t.TempDir()
	s, err := NewTemplateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	tpl, err := s.Register("Hello world, this is a longish prefix template.", "test", now)
	if err != nil {
		t.Fatal(err)
	}
	if tpl.Hash == "" || len(tpl.Hash) != templateHashLen {
		t.Fatalf("unexpected hash: %q", tpl.Hash)
	}

	hash, suffix, ok := s.Match("Hello world, this is a longish prefix template. with extra suffix")
	if !ok {
		t.Fatal("expected match")
	}
	if hash != tpl.Hash {
		t.Errorf("hash got %q want %q", hash, tpl.Hash)
	}
	if suffix != " with extra suffix" {
		t.Errorf("suffix got %q", suffix)
	}
}

func TestTemplateStore_NoMatch(t *testing.T) {
	s, _ := NewTemplateStore(t.TempDir())
	if _, _, ok := s.Match("anything"); ok {
		t.Fatal("expected no match on empty store")
	}
	s.Register("FOO BAR", "test", time.Now())
	if _, _, ok := s.Match("FOOX"); ok {
		t.Fatal("expected no match — diverges at char 3")
	}
}

func TestTemplateStore_LongestMatchWins(t *testing.T) {
	// Two templates where one is a strict prefix of the other. Match must
	// return the longer (more specific) so the suffix stays minimal.
	s, _ := NewTemplateStore(t.TempDir())
	short, _ := s.Register("[SUGGESTION MODE:", "test", time.Now())
	long, _ := s.Register("[SUGGESTION MODE: do the thing\nFIRST: think", "test", time.Now())
	hash, suffix, ok := s.Match("[SUGGESTION MODE: do the thing\nFIRST: think harder\nthen act")
	if !ok || hash != long.Hash {
		t.Fatalf("expected long template hash %q, got %q (short=%q)", long.Hash, hash, short.Hash)
	}
	if suffix != " harder\nthen act" {
		t.Errorf("suffix got %q", suffix)
	}
}

func TestTemplateStore_RegisterIsIdempotent(t *testing.T) {
	s, _ := NewTemplateStore(t.TempDir())
	t1, _ := s.Register("dup template body", "test", time.Now())
	t2, _ := s.Register("dup template body", "test", time.Now())
	if t1.Hash != t2.Hash {
		t.Fatalf("expected same hash, got %q vs %q", t1.Hash, t2.Hash)
	}
	if got := len(s.List()); got != 1 {
		t.Errorf("expected 1 template after duplicate Register, got %d", got)
	}
}

func TestTemplateStore_TouchUpdatesStats(t *testing.T) {
	s, _ := NewTemplateStore(t.TempDir())
	now := time.Now().UTC()
	tpl, _ := s.Register("body", "test", now)
	later := now.Add(5 * time.Minute)
	s.Touch(tpl.Hash, later)
	s.Touch(tpl.Hash, later.Add(time.Minute))
	got, _ := s.Get(tpl.Hash)
	if got.Occurrences != 2 {
		t.Errorf("occurrences got %d want 2", got.Occurrences)
	}
	if !got.LastSeen.Equal(later.Add(time.Minute)) {
		t.Errorf("last_seen got %v", got.LastSeen)
	}
}

func TestTemplateStore_TouchUnknownHashIsNoop(t *testing.T) {
	s, _ := NewTemplateStore(t.TempDir())
	s.Touch("deadbeefcafe", time.Now())
	if got := len(s.List()); got != 0 {
		t.Errorf("expected empty store, got %d", got)
	}
}

func TestTemplateStore_PersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewTemplateStore(dir)
	now := time.Now().UTC()
	tpl, _ := s.Register("persisted body", "test", now)
	s.Touch(tpl.Hash, now.Add(time.Second))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	// Reopen — should see the template + stats.
	s2, err := NewTemplateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Get(tpl.Hash)
	if !ok {
		t.Fatal("template missing after reload")
	}
	if got.Occurrences != 1 {
		t.Errorf("occurrences got %d want 1", got.Occurrences)
	}
	if _, _, ok := s2.Match("persisted body and more"); !ok {
		t.Errorf("trie not rebuilt on reload")
	}
}

func TestTemplateStore_HandlesUTF8(t *testing.T) {
	// Vietnamese / multibyte runes must not split mid-character. The trie is
	// rune-keyed, so a "bunh" prompt should not match a "bún" template.
	s, _ := NewTemplateStore(t.TempDir())
	tpl, _ := s.Register("Xin chào, bạn ", "test", time.Now())
	hash, suffix, ok := s.Match("Xin chào, bạn ơi muốn ăn gì")
	if !ok || hash != tpl.Hash {
		t.Fatalf("expected utf8 match, got ok=%v hash=%q", ok, hash)
	}
	if suffix != "ơi muốn ăn gì" {
		t.Errorf("suffix got %q", suffix)
	}
	if _, _, ok := s.Match("Xin chào, ban "); ok {
		t.Errorf("expected no match for ASCII 'ban' against unicode 'bạn'")
	}
}

func TestTemplateStore_SkipsMalformedLineOnLoad(t *testing.T) {
	dir := t.TempDir()
	good := Template{Hash: "abc123abc123", Length: 5, Text: "hello", FirstSeen: time.Now(), LastSeen: time.Now()}
	gb, _ := json.Marshal(good)
	contents := []byte("not-json\n")
	contents = append(contents, gb...)
	contents = append(contents, '\n')
	if err := os.WriteFile(filepath.Join(dir, templatesFileName), contents, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewTemplateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s.List()); got != 1 {
		t.Errorf("expected good line to load, got %d templates", got)
	}
}

func TestTemplateStore_AppendsOnRegister(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewTemplateStore(dir)
	s.Register("aaaa", "test", time.Now())
	s.Register("bbbb", "test", time.Now())
	data, err := os.ReadFile(filepath.Join(dir, templatesFileName))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "\n"); got != 2 {
		t.Errorf("expected 2 lines on disk, got %d", got)
	}
}
