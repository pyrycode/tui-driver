package tuidriver

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// failWriter is an io.Writer that always fails with err — drives the
// error-propagation paths of WriteHeader and Write.
type failWriter struct{ err error }

func (f failWriter) Write([]byte) (int, error) { return 0, f.err }

// oneLine asserts b holds exactly one '\n'-terminated line and returns the
// line with the trailing newline stripped. asciinema v2 is line-delimited,
// so every header/event must be exactly one such line.
func oneLine(t *testing.T, b []byte) []byte {
	t.Helper()
	if len(b) == 0 {
		t.Fatalf("output is empty, want one newline-terminated line")
	}
	if b[len(b)-1] != '\n' {
		t.Fatalf("output %q is not newline-terminated", b)
	}
	if n := bytes.Count(b, []byte{'\n'}); n != 1 {
		t.Fatalf("output has %d newlines, want exactly 1: %q", n, b)
	}
	return b[:len(b)-1]
}

// eventElapsed parses b as a single asciinema v2 event line and returns its
// elapsed field, failing the test if the line is not a 3-element array.
func eventElapsed(t *testing.T, b []byte) float64 {
	t.Helper()
	line := oneLine(t, b)
	var ev []any
	if err := json.Unmarshal(line, &ev); err != nil {
		t.Fatalf("event %q is not a JSON array: %v", line, err)
	}
	if len(ev) != 3 {
		t.Fatalf("event has %d elements, want 3: %q", len(ev), line)
	}
	elapsed, ok := ev[0].(float64)
	if !ok {
		t.Fatalf("event[0] elapsed = %v (%T), want float64", ev[0], ev[0])
	}
	return elapsed
}

func TestCastRecorderWriteHeader(t *testing.T) {
	var buf bytes.Buffer
	r := NewCastRecorder(&buf, 80, 24)

	if err := r.WriteHeader(); err != nil {
		t.Fatalf("WriteHeader() error = %v", err)
	}

	line := oneLine(t, buf.Bytes())
	var hdr map[string]any
	if err := json.Unmarshal(line, &hdr); err != nil {
		t.Fatalf("header %q is not a JSON object: %v", line, err)
	}
	if hdr["version"] != float64(2) {
		t.Errorf("header version = %v, want 2", hdr["version"])
	}
	if hdr["width"] != float64(80) {
		t.Errorf("header width = %v, want 80", hdr["width"])
	}
	if hdr["height"] != float64(24) {
		t.Errorf("header height = %v, want 24", hdr["height"])
	}
}

// TestCastRecorderWriteHeaderPropagatesError pins the AC's "a failed
// underlying write surfaces to the caller rather than being swallowed".
func TestCastRecorderWriteHeaderPropagatesError(t *testing.T) {
	sentinel := errors.New("disk full")
	r := NewCastRecorder(failWriter{err: sentinel}, 80, 24)

	if err := r.WriteHeader(); !errors.Is(err, sentinel) {
		t.Errorf("WriteHeader() error = %v, want %v", err, sentinel)
	}
}

func TestCastRecorderWriteEventShapeAndRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	r := NewCastRecorder(&buf, 80, 24)

	// A payload mixing the tricky-but-valid cases: double-quote, backslash,
	// newline, tab, ESC, the HTML-escape triggers (< > &), and multibyte
	// runes. All are valid UTF-8, so encoding/json must round-trip them
	// byte-for-byte (HTML escapes like < decode back to the same byte).
	payload := []byte("a\"b\\c\nd\te\x1bf <g> &h café 🚀")

	n, err := r.Write(payload)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != len(payload) {
		t.Errorf("Write() n = %d, want %d", n, len(payload))
	}

	line := oneLine(t, buf.Bytes())
	var ev []any
	if err := json.Unmarshal(line, &ev); err != nil {
		t.Fatalf("event %q is not a JSON array: %v", line, err)
	}
	if len(ev) != 3 {
		t.Fatalf("event has %d elements, want 3: %q", len(ev), line)
	}
	if _, ok := ev[0].(float64); !ok {
		t.Errorf("event[0] elapsed = %v (%T), want float64", ev[0], ev[0])
	}
	if ev[1] != "o" {
		t.Errorf("event[1] code = %v, want %q", ev[1], "o")
	}
	data, ok := ev[2].(string)
	if !ok {
		t.Fatalf("event[2] data = %v (%T), want string", ev[2], ev[2])
	}
	if !bytes.Equal([]byte(data), payload) {
		t.Errorf("round-trip mismatch:\n got %q\nwant %q", data, payload)
	}
}

// TestCastRecorderClockStartsOnFirstWrite proves neither construction nor
// WriteHeader starts the elapsed clock: after a measurable gap, the first
// event's elapsed is still ~0.
func TestCastRecorderClockStartsOnFirstWrite(t *testing.T) {
	var buf bytes.Buffer
	r := NewCastRecorder(&buf, 80, 24)

	if err := r.WriteHeader(); err != nil {
		t.Fatalf("WriteHeader() error = %v", err)
	}
	buf.Reset() // drop the header line; keep only the first event below

	// If the clock started at construction or in WriteHeader, the first
	// event's elapsed would reflect this delay; lazy start keeps it ~0.
	time.Sleep(20 * time.Millisecond)
	if _, err := r.Write([]byte("first")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if elapsed := eventElapsed(t, buf.Bytes()); elapsed > 0.01 {
		t.Errorf("first event elapsed = %v s, want ~0 (<0.01); clock did not start lazily on first Write", elapsed)
	}
}

// TestCastRecorderLaterEventHasLargerElapsed pins that elapsed advances: an
// event written after a measurable delay has a strictly larger elapsed than
// the first.
func TestCastRecorderLaterEventHasLargerElapsed(t *testing.T) {
	var buf bytes.Buffer
	r := NewCastRecorder(&buf, 80, 24)

	if _, err := r.Write([]byte("first")); err != nil {
		t.Fatalf("first Write() error = %v", err)
	}
	first := eventElapsed(t, buf.Bytes())

	buf.Reset()
	time.Sleep(5 * time.Millisecond)
	if _, err := r.Write([]byte("second")); err != nil {
		t.Fatalf("second Write() error = %v", err)
	}
	second := eventElapsed(t, buf.Bytes())

	if second <= first {
		t.Errorf("second elapsed %v not strictly greater than first %v", second, first)
	}
}

// TestCastRecorderWritePropagatesError pins the io.Writer contract on the
// failure path: a failed underlying write returns (0, err), not a swallowed
// success.
func TestCastRecorderWritePropagatesError(t *testing.T) {
	sentinel := errors.New("disk full")
	r := NewCastRecorder(failWriter{err: sentinel}, 80, 24)

	n, err := r.Write([]byte("data"))
	if !errors.Is(err, sentinel) {
		t.Errorf("Write() error = %v, want %v", err, sentinel)
	}
	if n != 0 {
		t.Errorf("Write() n = %d on error, want 0", n)
	}
}
