package promptlog

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// newCapturer wraps a fresh httptest.ResponseRecorder via Gin so the
// embedded ResponseWriter has the full Gin interface (Status, Size, etc.).
func newCapturerForTest(cap int) (*responseCapturer, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	return newResponseCapturer(c.Writer, cap), rec
}

func TestResponseCapturer_BuffersUnderCap(t *testing.T) {
	cap_, rec := newCapturerForTest(100)
	in := []byte("hello world")
	n, err := cap_.Write(in)
	if err != nil || n != len(in) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	if got := cap_.Body(); !bytes.Equal(got, in) {
		t.Errorf("buf=%q want %q", got, in)
	}
	if cap_.Truncated() {
		t.Error("should not be truncated below cap")
	}
	if rec.Body.String() != "hello world" {
		t.Errorf("downstream lost write: %q", rec.Body.String())
	}
}

func TestResponseCapturer_TruncatesAtCap(t *testing.T) {
	cap_, rec := newCapturerForTest(5)
	cap_.Write([]byte("abc"))
	cap_.Write([]byte("defghij"))
	cap_.Write([]byte("klm"))
	if got := string(cap_.Body()); got != "abcde" {
		t.Errorf("buf=%q want abcde", got)
	}
	if !cap_.Truncated() {
		t.Error("expected truncated")
	}
	// Downstream still gets every byte — client stream is never short-cut.
	if rec.Body.String() != "abcdefghijklm" {
		t.Errorf("downstream truncated: %q", rec.Body.String())
	}
}

func TestResponseCapturer_WriteString(t *testing.T) {
	cap_, rec := newCapturerForTest(100)
	cap_.WriteString("foo")
	cap_.WriteString("bar")
	if got := string(cap_.Body()); got != "foobar" {
		t.Errorf("buf=%q", got)
	}
	if rec.Body.String() != "foobar" {
		t.Errorf("downstream: %q", rec.Body.String())
	}
}

func TestResponseCapturer_ZeroCapNoOp(t *testing.T) {
	cap_, _ := newCapturerForTest(0)
	cap_.Write([]byte("anything"))
	if len(cap_.Body()) != 0 {
		t.Errorf("zero cap should not buffer, got %q", cap_.Body())
	}
	if !cap_.Truncated() {
		t.Error("zero cap with a write should mark truncated")
	}
}
