package tuidriver

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// CastRecorder mirrors a PTY byte stream to an asciinema v2 recording. It is
// an io.Writer used internally by SpawnOpts.RecordTo: the reader goroutine
// frames every PTY read as one asciinema "o" (output) event, producing a .cast
// file
// that can be replayed (asciinema play) or parsed offline.
//
// The wire format is line-delimited JSON:
//
//	{"version":2,"width":80,"height":24}
//	[0.248,"o","[2J[Hhello"]
//
// WriteHeader emits the header line once, before the first Write; each Write
// emits one event line. The elapsed clock starts lazily on the first Write
// (not at construction or WriteHeader), so the first event's elapsed is ~0
// regardless of the gap between setup and the first byte.
//
// Data encoding: each chunk is recorded as string(p) marshalled with
// encoding/json, which escapes quotes, backslashes, and control bytes
// correctly. Valid UTF-8 (including multibyte runes and control/escape
// bytes) round-trips exactly. Invalid UTF-8 is coerced to U+FFFD by
// json.Marshal, which is acceptable for a recorder: asciinema assumes a
// UTF-8 text stream and real claude output is UTF-8. The recorder copies
// raw bytes verbatim with no redaction — a .cast may contain anything the
// terminal showed; placement and permissions are the consumer's call.
//
// Documented use is a single writer (the PTY mirror goroutine). The mutex
// guards the lazy clock start and the underlying write, so concurrent
// writers stay safe and correctly serialised, but that is defence-in-depth,
// not a required guarantee. The recorder owns no lifecycle: the consumer
// owns the underlying io.Writer and closes it.
//
// Construct with NewCastRecorder. The zero value is not usable.
type CastRecorder struct {
	w     io.Writer
	cols  int
	rows  int
	mu    sync.Mutex
	start time.Time // zero until the first Write; set under mu
}

// castHeader is the asciinema v2 header object. A struct (rather than a map)
// fixes key order, keeping the emitted header deterministic. timestamp,
// title, and env are optional and intentionally omitted — the AC tolerates
// their absence and omitting them keeps the header free of time.Now().
type castHeader struct {
	Version int `json:"version"`
	Width   int `json:"width"`
	Height  int `json:"height"`
}

// Compile-time assertion that *CastRecorder satisfies io.Writer so the reader
// goroutine can tee PTY bytes to it.
var _ io.Writer = (*CastRecorder)(nil)

// NewCastRecorder returns a recorder that writes an asciinema v2 cast to w
// using cols/rows as the recorded terminal dimensions. It does not write
// anything and does not start the clock — call WriteHeader once, then tee PTY
// bytes to it. Used internally by SpawnOpts.RecordTo.
func NewCastRecorder(w io.Writer, cols, rows int) *CastRecorder {
	return &CastRecorder{w: w, cols: cols, rows: rows}
}

// WriteHeader writes the asciinema v2 header line (one newline-terminated
// JSON object carrying version 2 and the constructor's width/height). Call
// it once, before the first Write. The underlying write error, if any, is
// returned rather than swallowed.
func (r *CastRecorder) WriteHeader() error {
	line, err := json.Marshal(castHeader{Version: 2, Width: r.cols, Height: r.rows})
	if err != nil {
		return err
	}
	line = append(line, '\n')

	r.mu.Lock()
	defer r.mu.Unlock()
	_, err = r.w.Write(line)
	return err
}

// Write frames p as one asciinema v2 output event and appends it as a single
// newline-terminated line: [elapsed, "o", string(p)]. The elapsed clock
// starts lazily on this first call. On success it returns (len(p), nil); on
// a failed underlying write it returns (0, err) — recording is all-or-nothing
// per chunk, so 0 is the honest count of bytes durably recorded.
//
// p is consumed synchronously (string(p) copies during marshal), so the
// caller may reuse its buffer once Write returns — which the Session reader
// relies on, since the mirrored chunk aliases its reusable read buffer.
func (r *CastRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.start.IsZero() {
		r.start = time.Now()
	}
	elapsed := time.Since(r.start).Seconds()

	line, err := json.Marshal([]any{elapsed, "o", string(p)})
	if err != nil {
		return 0, err
	}
	line = append(line, '\n')

	if _, err := r.w.Write(line); err != nil {
		return 0, err
	}
	return len(p), nil
}
