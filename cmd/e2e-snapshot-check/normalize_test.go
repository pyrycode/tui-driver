package main

import (
	"bytes"
	"testing"
)

// tryLine builds a synthetic Try-line byte sequence matching the structure
// claude 2.1.148+ emits in the picker. The prompt text rotates per
// invocation; the surrounding escape framing does not.
func tryLine(prompt string) []byte {
	var buf bytes.Buffer
	buf.WriteString("\x1b[7mT\x1b[27m\x1b[2mry\x1b[7G\"")
	buf.WriteString(prompt)
	buf.WriteString("\"\x1b[22m")
	return buf.Bytes()
}

func TestNormalize_DifferentPromptsCollapse(t *testing.T) {
	a := tryLine("edit go.mod to...")
	b := tryLine("how does trust_test.go work?")

	if bytes.Equal(a, b) {
		t.Fatal("test setup invalid: inputs already equal before normalisation")
	}
	if !bytes.Equal(normalize(a), normalize(b)) {
		t.Errorf("two Try-line variants did not normalise to equal bytes:\n a=%q\n b=%q", normalize(a), normalize(b))
	}
}

func TestNormalize_NoTryLineUnchanged(t *testing.T) {
	in := []byte("\x1b[38;2;153;153;153m/advisor\x1b[44GConfigure\x1b[39m")
	out := normalize(in)
	if !bytes.Equal(in, out) {
		t.Errorf("input without Try line was modified:\n in =%q\n out=%q", in, out)
	}
}

func TestNormalize_Idempotent(t *testing.T) {
	in := append([]byte("prefix-bytes\r\n"), tryLine("edit go.mod to...")...)
	in = append(in, []byte("\r\nsuffix-bytes")...)

	once := normalize(in)
	twice := normalize(once)
	if !bytes.Equal(once, twice) {
		t.Errorf("normalize is not idempotent:\n once =%q\n twice=%q", once, twice)
	}
}

func TestNormalize_LeavesSurroundingBytesIntact(t *testing.T) {
	prefix := []byte("\x1b[1mClaude\x1b[19GCode\x1b[22m\r\n")
	suffix := []byte("\r\n\x1b[38;2;153;153;153m?\x1b[39m")
	in := append(append([]byte{}, prefix...), tryLine("edit go.mod to...")...)
	in = append(in, suffix...)

	out := normalize(in)

	if !bytes.HasPrefix(out, prefix) {
		t.Errorf("prefix changed by normalisation:\n want prefix=%q\n got    out  =%q", prefix, out)
	}
	if !bytes.HasSuffix(out, suffix) {
		t.Errorf("suffix changed by normalisation:\n want suffix=%q\n got    out  =%q", suffix, out)
	}
	if bytes.Contains(out, []byte("edit go.mod")) {
		t.Errorf("normalisation did not mask the prompt content; out=%q", out)
	}
}

func TestNormalize_DoesNotMutateInput(t *testing.T) {
	in := append([]byte("prefix"), tryLine("edit go.mod to...")...)
	clone := append([]byte{}, in...)
	_ = normalize(in)
	if !bytes.Equal(in, clone) {
		t.Errorf("normalize mutated its input:\n before=%q\n after =%q", clone, in)
	}
}
