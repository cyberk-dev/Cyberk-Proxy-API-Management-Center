package promptlog

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateText_Short(t *testing.T) {
	got, trunc, orig := truncateText("hello", 100)
	if got != "hello" || trunc || orig != 5 {
		t.Errorf("got=%q trunc=%v orig=%d", got, trunc, orig)
	}
}

func TestTruncateText_Long(t *testing.T) {
	in := strings.Repeat("a", 1000) + strings.Repeat("b", 1000)
	got, trunc, orig := truncateText(in, 200)
	if !trunc {
		t.Fatal("expected truncated")
	}
	if orig != 2000 {
		t.Errorf("orig=%d", orig)
	}
	if !strings.HasPrefix(got, "aaaa") {
		t.Errorf("head lost: %q", got[:20])
	}
	if !strings.HasSuffix(got, "bbbb") {
		t.Errorf("tail lost")
	}
	if !strings.Contains(got, "[truncated") {
		t.Errorf("marker missing: %q", got)
	}
}

func TestTruncateText_UTF8Safe(t *testing.T) {
	// 200 copies of a 3-byte rune (世) = 600 bytes. Truncate to 100 bytes
	// must not split a rune.
	in := strings.Repeat("世", 200)
	got, trunc, _ := truncateText(in, 100)
	if !trunc {
		t.Fatal("expected truncated")
	}
	if !utf8.ValidString(got) {
		t.Errorf("invalid UTF-8 after truncation: %q", got)
	}
}

func TestMaskBase64_DecodesAndHashes(t *testing.T) {
	payload := []byte("hello world")
	enc := base64.StdEncoding.EncodeToString(payload)
	n, sha := maskBase64(enc)
	if n != len(payload) {
		t.Errorf("bytes=%d want %d", n, len(payload))
	}
	if len(sha) != 16 {
		t.Errorf("sha length=%d want 16 hex chars", len(sha))
	}
	// Stability: same input → same hash.
	n2, sha2 := maskBase64(enc)
	if n2 != n || sha2 != sha {
		t.Errorf("not deterministic")
	}
}

func TestMaskBase64_Empty(t *testing.T) {
	n, sha := maskBase64("")
	if n != 0 || sha != "" {
		t.Errorf("empty: bytes=%d sha=%q", n, sha)
	}
}

func TestMaskBase64_Garbage(t *testing.T) {
	// Not valid base64 — should not panic, returns length of raw string
	// and empty hash.
	n, sha := maskBase64("!!!not-base64!!!")
	if n == 0 {
		t.Errorf("expected non-zero bytes fallback")
	}
	_ = sha
}

func TestParseDataURL_OK(t *testing.T) {
	mt, payload, ok := parseDataURL("data:image/png;base64,aGVsbG8=")
	if !ok {
		t.Fatal("expected ok")
	}
	if mt != "image/png" {
		t.Errorf("mt=%q", mt)
	}
	if payload != "aGVsbG8=" {
		t.Errorf("payload=%q", payload)
	}
}

func TestParseDataURL_NotDataURL(t *testing.T) {
	_, _, ok := parseDataURL("https://example.com/foo.png")
	if ok {
		t.Fatal("expected not-ok for https url")
	}
}

func TestParseDataURL_NonBase64(t *testing.T) {
	// data: URL without base64 marker — treated as not-ok since chat APIs
	// don't use raw text data URLs.
	_, _, ok := parseDataURL("data:text/plain,hello")
	if ok {
		t.Fatal("expected not-ok for non-base64 data url")
	}
}
