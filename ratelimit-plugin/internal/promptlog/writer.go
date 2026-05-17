package promptlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// Entry is one record in the JSONL log: a single user message extracted from
// one inbound request, plus the contextual metadata needed to make sense of
// it offline. Status is filled in after the downstream handler returns, so a
// rejected request shows the rejection status (400/429) instead of 200 —
// useful when correlating spam patterns with policy / rate-limit hits.
type Entry struct {
	Timestamp time.Time `json:"ts"`
	Provider  string    `json:"provider"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
	Model     string    `json:"model,omitempty"`
	KeyHash   string    `json:"key_hash"`

	// Role distinguishes a captured user prompt ("user", omitted for legacy
	// entries written before assistant-side logging existed — readers treat
	// empty as "user") from a captured assistant response ("assistant").
	// Both roles share the same session_id and key_hash so the UI can pair
	// them chronologically into a chat-style view.
	Role string `json:"role,omitempty"`

	// Client-side metadata (from headers). Client is always set; the rest are
	// best-effort and omitted when the source headers are missing.
	Client        string `json:"client"`
	ClientVersion string `json:"client_version,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	CWD           string `json:"cwd,omitempty"`

	// Prompt is the joined human-readable text for quick grep / dashboarding.
	// When PromptTemplate is set, Prompt holds only the SUFFIX after the
	// template prefix; reconstruct the original by concatenating the
	// template body + Prompt. Blocks is the structured detail (typed
	// blocks, image metadata, etc) and is NEVER templated.
	Prompt         string  `json:"prompt,omitempty"`
	PromptTemplate string  `json:"prompt_template,omitempty"`
	Blocks         []Block `json:"blocks"`

	BodyTruncated bool `json:"body_truncated,omitempty"`
}

// Writer serializes Entries to a daily-rotated JSONL file in a background
// goroutine. Submit is non-blocking: if the queue is full the entry is
// dropped (counted via Dropped) rather than back-pressuring the request path.
// Losing a log line in exchange for never blocking a real user request is the
// right trade-off for analytics logging.
//
// When templates is non-nil, each entry's prompt is matched against the
// store before encoding: a hit replaces the prefix with the template hash
// (Entry.PromptTemplate) and shortens Entry.Prompt to the suffix only. This
// happens on the writer goroutine so the request path stays free of any
// template-related cost beyond the channel send.
type Writer struct {
	dir       string
	ch        chan *Entry
	wg        sync.WaitGroup
	templates *TemplateStore
	detector  *templateDetector

	dropped atomic.Uint64

	closeOnce sync.Once
	closed    atomic.Bool
}

// NewWriter opens (or creates) dir and starts the background flush
// goroutine. The directory is created with 0755 if missing; an unwritable dir
// is reported as an error so misconfiguration is loud at startup rather than
// silently swallowed at request time.
//
// templates may be nil to disable prompt-templating. When non-nil, the
// writer also seeds and runs a templateDetector with cfg.Templates.
func NewWriter(dir string, queueSize int, templates *TemplateStore, cfg TemplatesConfig) (*Writer, error) {
	if dir == "" {
		return nil, fmt.Errorf("promptlog: empty dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("promptlog: mkdir %s: %w", dir, err)
	}
	if queueSize <= 0 {
		queueSize = defaultQueueSize
	}
	w := &Writer{
		dir:       dir,
		ch:        make(chan *Entry, queueSize),
		templates: templates,
	}
	if templates != nil && cfg.Enabled {
		w.detector = newTemplateDetector(templates, cfg)
	}
	w.wg.Add(1)
	go w.run()
	return w, nil
}

// Submit enqueues e for asynchronous write. Returns true when accepted,
// false when the queue was full or the writer is closed. Callers should not
// retry on false — the contract is "best effort, never block."
func (w *Writer) Submit(e *Entry) bool {
	if w == nil || e == nil {
		return false
	}
	if w.closed.Load() {
		return false
	}
	select {
	case w.ch <- e:
		return true
	default:
		w.dropped.Add(1)
		return false
	}
}

// Dropped returns the count of entries discarded due to a full queue. Exposed
// so operators can monitor for sustained overrun (a sign queue_size should
// grow or write throughput is too low).
func (w *Writer) Dropped() uint64 {
	if w == nil {
		return 0
	}
	return w.dropped.Load()
}

// Close stops the background goroutine, flushes the current file, and waits
// for it to exit. Safe to call multiple times. Also flushes the template
// store's stats (occurrence/last-seen) — appended templates are durable
// already, but Touch() updates only land on disk via Flush.
func (w *Writer) Close() {
	if w == nil {
		return
	}
	w.closeOnce.Do(func() {
		w.closed.Store(true)
		close(w.ch)
	})
	w.wg.Wait()
	if w.templates != nil {
		if err := w.templates.Flush(); err != nil {
			log.Warnf("promptlog: templates flush on close: %v", err)
		}
	}
}

func (w *Writer) run() {
	defer w.wg.Done()

	var (
		currentDate string
		file        *os.File
		buf         *bufio.Writer
	)

	closeFile := func() {
		if buf != nil {
			if err := buf.Flush(); err != nil {
				log.Warnf("promptlog: flush: %v", err)
			}
			buf = nil
		}
		if file != nil {
			if err := file.Close(); err != nil {
				log.Warnf("promptlog: close: %v", err)
			}
			file = nil
		}
	}
	defer closeFile()

	ensureFile := func(date string) bool {
		if date == currentDate && file != nil {
			return true
		}
		closeFile()
		path := filepath.Join(w.dir, fmt.Sprintf("prompts-%s.jsonl", date))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.Warnf("promptlog: open %s: %v", path, err)
			return false
		}
		file = f
		// 64 KiB is large enough that bursty writes (chat-completions flood)
		// don't syscall per entry, small enough that a 2s flush keeps log
		// loss to seconds, not minutes, on a crash.
		buf = bufio.NewWriterSize(file, 64*1024)
		currentDate = date
		return true
	}

	flushTicker := time.NewTicker(2 * time.Second)
	defer flushTicker.Stop()
	// Templates flush on a slower cadence — Touch updates only count
	// statistics, so 60 s of churn loss on crash is acceptable, and
	// rewriting the whole file every 2 s would dominate I/O once the
	// catalog grows past a few hundred templates.
	tplFlushTicker := time.NewTicker(60 * time.Second)
	defer tplFlushTicker.Stop()

	encode := json.Marshal

	for {
		select {
		case e, ok := <-w.ch:
			if !ok {
				return
			}
			ts := e.Timestamp
			if ts.IsZero() {
				ts = time.Now()
			}
			// Templating must run BEFORE encode so PromptTemplate / shortened
			// Prompt land in the JSONL line. Detector observes the original
			// (untemplated) prompt to find new patterns; ordering matters
			// only for the entry written to disk.
			if w.detector != nil {
				w.detector.observe(e.Prompt, ts)
			}
			if w.templates != nil {
				if hash, suffix, hit := w.templates.Match(e.Prompt); hit {
					e.PromptTemplate = hash
					e.Prompt = suffix
					w.templates.Touch(hash, ts)
				}
			}
			// Strip Block.Text on every block whose content already lives in
			// Entry.Prompt (joinPromptText concatenated it in, possibly via
			// summarizeNonText for tool / thinking blocks). Keeping it
			// per-block duplicated ~45% of every record before the strip
			// landed. Block.Bytes preserves the original length so consumers
			// can still see how big each contribution was. Image / document
			// / audio carry no Text — the strip is a no-op for them.
			for i := range e.Blocks {
				if e.Blocks[i].Text == "" {
					continue
				}
				if e.Blocks[i].Bytes == 0 {
					e.Blocks[i].Bytes = len(e.Blocks[i].Text)
				}
				e.Blocks[i].Text = ""
			}
			date := ts.UTC().Format("2006-01-02")
			if !ensureFile(date) {
				continue
			}
			b, err := encode(e)
			if err != nil {
				log.Warnf("promptlog: marshal: %v", err)
				continue
			}
			if _, err := buf.Write(b); err != nil {
				log.Warnf("promptlog: write: %v", err)
				continue
			}
			if err := buf.WriteByte('\n'); err != nil {
				log.Warnf("promptlog: write: %v", err)
			}
		case <-flushTicker.C:
			if buf != nil {
				if err := buf.Flush(); err != nil {
					log.Warnf("promptlog: periodic flush: %v", err)
				}
			}
		case <-tplFlushTicker.C:
			if w.templates != nil {
				if err := w.templates.Flush(); err != nil {
					log.Warnf("promptlog: templates periodic flush: %v", err)
				}
			}
		}
	}
}
