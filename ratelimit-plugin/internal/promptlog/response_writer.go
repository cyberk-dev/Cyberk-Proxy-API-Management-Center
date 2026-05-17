package promptlog

import (
	"bytes"

	"github.com/gin-gonic/gin"
)

// responseCapturer wraps gin.ResponseWriter so promptlog can observe the
// upstream response body without interfering with the live client stream.
// Bytes are forwarded to the embedded writer first (zero added latency for
// the caller); a copy lands in buf until cap bytes have accumulated, after
// which further writes pass through untouched and Truncated stays true.
//
// The wrapper does NOT decode SSE or split frames — it is intentionally
// content-type agnostic. Parsing (provider-specific, streaming or not) runs
// after c.Next() in the middleware, against whatever bytes the buffer
// captured. That separation lets the hot path stay a simple memcpy.
type responseCapturer struct {
	gin.ResponseWriter
	buf       *bytes.Buffer
	cap       int
	written   int
	truncated bool
}

// newResponseCapturer wires a fresh buffer in front of w. cap must be > 0;
// callers that want capture disabled should skip the wrap entirely instead
// of passing zero. The buffer is pre-grown to a modest 8 KiB (or `cap`,
// whichever is smaller) so a typical short reply lands in one allocation
// and a long SSE stream avoids the bytes.Buffer doubling dance.
func newResponseCapturer(w gin.ResponseWriter, cap int) *responseCapturer {
	buf := &bytes.Buffer{}
	preGrow := 8 * 1024
	if cap > 0 && cap < preGrow {
		preGrow = cap
	}
	if preGrow > 0 {
		buf.Grow(preGrow)
	}
	return &responseCapturer{
		ResponseWriter: w,
		buf:            buf,
		cap:            cap,
	}
}

// Write tees up to (cap - written) bytes into the buffer, then forwards the
// full slice downstream. The split-only-if-cap-spans logic mirrors how the
// SDK's own response writer behaves: never short-write to the client even if
// the buffer fills mid-frame.
func (r *responseCapturer) Write(p []byte) (int, error) {
	r.captureSlice(p)
	return r.ResponseWriter.Write(p)
}

// WriteString satisfies gin.ResponseWriter; same tee semantics as Write.
func (r *responseCapturer) WriteString(s string) (int, error) {
	r.captureSlice([]byte(s))
	return r.ResponseWriter.WriteString(s)
}

func (r *responseCapturer) captureSlice(p []byte) {
	if r.cap <= 0 || r.written >= r.cap {
		if len(p) > 0 {
			r.truncated = true
		}
		return
	}
	room := r.cap - r.written
	if len(p) <= room {
		r.buf.Write(p)
		r.written += len(p)
		return
	}
	r.buf.Write(p[:room])
	r.written += room
	r.truncated = true
}

// Body returns the captured prefix of the response. Caller must not mutate.
func (r *responseCapturer) Body() []byte { return r.buf.Bytes() }

// Truncated reports whether any bytes were dropped because the buffer cap
// was reached. Stored as Entry.BodyTruncated for the assistant entry.
func (r *responseCapturer) Truncated() bool { return r.truncated }
