package effortnormalize

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/cyberk/ratelimit-plugin/internal/ratelimit"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// echoBody returns the request body the downstream handler actually received,
// so tests can assert whether (and how) the middleware mutated it.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	r := gin.New()
	// Prime the peek cache as production would (promptlog runs first and calls
	// PeekJSONBody). Without this, the middleware peeks from a body that's
	// already drained by the handler chain.
	r.Use(func(c *gin.Context) {
		ratelimit.PeekJSONBody(c)
		c.Next()
	})
	r.Use(Middleware())
	echo := func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		c.Data(http.StatusOK, "application/json", body)
	}
	r.POST("/v1/responses", echo)
	r.POST("/v1/chat/completions", echo)
	r.POST("/v1/messages", echo)
	return httptest.NewServer(r)
}

func postJSON(t *testing.T, url, body string) []byte {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, got)
	}
	return got
}

func TestMiddleware_RewritesMinimalToLow(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	in := `{"model":"gpt-5-nano","reasoning":{"effort":"minimal"},"input":[]}`
	got := postJSON(t, srv.URL+"/v1/responses", in)

	if eff := gjson.GetBytes(got, "reasoning.effort").String(); eff != "low" {
		t.Errorf("reasoning.effort: want %q, got %q (body=%s)", "low", eff, got)
	}
	// Sibling fields must be preserved.
	if model := gjson.GetBytes(got, "model").String(); model != "gpt-5-nano" {
		t.Errorf("model: want %q, got %q", "gpt-5-nano", model)
	}
}

func TestMiddleware_LeavesOtherEffortLevelsAlone(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	for _, level := range []string{"low", "medium", "high", "xhigh"} {
		in := `{"model":"gpt-5.5","reasoning":{"effort":"` + level + `"}}`
		got := postJSON(t, srv.URL+"/v1/chat/completions", in)
		if eff := gjson.GetBytes(got, "reasoning.effort").String(); eff != level {
			t.Errorf("level=%q: middleware mutated to %q", level, eff)
		}
	}
}

func TestMiddleware_NoReasoningFieldIsPassthrough(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	in := `{"model":"claude-opus-4-5","messages":[{"role":"user","content":"hi"}]}`
	got := postJSON(t, srv.URL+"/v1/messages", in)
	if !bytes.Equal(got, []byte(in)) {
		t.Errorf("body mutated: want %q, got %q", in, got)
	}
}

func TestMiddleware_NonJSONIsPassthrough(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/responses", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "not json" {
		t.Errorf("non-json mutated: %q", got)
	}
}

func TestMiddleware_PreservesSiblingReasoningFields(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	// reasoning.effort gets rewritten; reasoning.summary must survive.
	in := `{"model":"gpt-5-nano","reasoning":{"effort":"minimal","summary":"auto"}}`
	got := postJSON(t, srv.URL+"/v1/responses", in)

	if eff := gjson.GetBytes(got, "reasoning.effort").String(); eff != "low" {
		t.Errorf("effort: want low, got %q", eff)
	}
	if summary := gjson.GetBytes(got, "reasoning.summary").String(); summary != "auto" {
		t.Errorf("summary: want %q, got %q (body=%s)", "auto", summary, got)
	}
}
