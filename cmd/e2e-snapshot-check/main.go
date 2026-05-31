// e2e-snapshot-check re-derives each committed fixture under
// pkg/tuidriver/testdata/ by running spike-multiselect with the appropriate
// -trigger and -settle flags, then compares the resulting parsed-shape JSON
// dump against the committed fixture. Exits 0 iff every fixture matched.
//
// Spec 81 (supersedes spec 35): comparison is parsed-shape JSON (via the
// .parsed.json file spike-multiselect writes alongside its raw .bin dump),
// not raw PTY bytes. Equality is reflect.DeepEqual over the json.Unmarshal'd
// trees to survive incidental whitespace / key-order differences. The
// renderer-byte volatility (Try-line rotation, bimodal whitespace-clear
// passes — see #72) is absorbed by the parsers, so the parsed shape is
// stable where the byte stream is not.
//
// The check binary unconditionally sets TUIDRIVER_STRICT_MCP_CONFIG=1 in the
// env passed to every spike-multiselect child so re-recorded fixtures don't
// leak operator MCP-config paths into the mcp fixture (idempotent when
// `make e2e` already set it; load-bearing for `bin/e2e-snapshot-check`
// direct invocation).
//
// Stdout protocol (one line per fixture, in fixture-table order):
//
//	SNAPSHOT mcp match|diff|recorded
//	SNAPSHOT agents match|diff|recorded
//
// Stderr carries operator-facing diagnostics (mirrored spike-multiselect
// output, per-fixture "drift in <path>" lines on diff, plus a
// "captured at <parsed.json>" forensic anchor for `diff`).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
)

// dumpPathRe scrapes spike-multiselect's stderr "picker-snapshot path=<path> raw_len=... stripped_len=..."
// log line. The label is "picker-snapshot" regardless of which trigger we used.
var dumpPathRe = regexp.MustCompile(`picker-snapshot path=(\S+)`)

// modalClassRe scrapes spike-multiselect's stderr "modal-class detected=<X>"
// log line (logged before the parser dispatch, so it's present even when no
// sidecar is written). `(\S*)` not `\S+`: ModalClassUnknown is the empty
// string, so the line is "modal-class detected=" with nothing after it. Used
// to enrich the missing-sidecar diff message — a missing .parsed.json with a
// non-MCP class is classifier drift, not a content diff (#128).
var modalClassRe = regexp.MustCompile(`modal-class detected=(\S*)`)

type fixture struct {
	name    string // "mcp" | "agents"
	trigger string // raw bytes for -trigger flag
	settle  time.Duration
}

func main() {
	binDir := flag.String("bin-dir", "./bin", "directory containing the built spike-multiselect binary")
	testdataDir := flag.String("testdata-dir", "./pkg/tuidriver/testdata", "directory containing committed *-snapshot.json fixtures")
	spikeTimeout := flag.Duration("spike-timeout", 60*time.Second, "per-fixture spike-multiselect wall budget")
	record := flag.Bool("record", false, "overwrite committed fixtures with freshly-captured parsed JSON instead of comparing")
	flag.Parse()

	fixtures := []fixture{
		{name: "mcp", trigger: "/mcp\r", settle: 5 * time.Second},
		{name: "agents", trigger: "/agents\r", settle: 0},
	}

	allOK := true
	for _, f := range fixtures {
		ok := runFixture(*binDir, *testdataDir, *spikeTimeout, *record, f)
		if !ok {
			allOK = false
		}
	}
	if !allOK {
		os.Exit(1)
	}
}

func runFixture(binDir, testdataDir string, spikeTimeout time.Duration, record bool, f fixture) bool {
	fixturePath := filepath.Join(testdataDir, f.name+"-snapshot.json")

	ctx, cancel := context.WithTimeout(context.Background(), spikeTimeout)
	defer cancel()

	args := []string{"-trust-folder=accept", "-trigger=" + f.trigger}
	if f.settle > 0 {
		args = append(args, "-settle="+f.settle.String())
	}
	binPath := filepath.Join(binDir, "spike-multiselect")

	cmd := exec.CommandContext(ctx, binPath, args...)
	// Force strict-mcp on every child so re-recorded fixtures are
	// reproducible across hosts. Idempotent when the parent env already
	// has it set (the e2e-runner does); load-bearing for direct invocation.
	cmd.Env = append(os.Environ(), "TUIDRIVER_STRICT_MCP_CONFIG=1")
	var stderrBuf bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = io.MultiWriter(&stderrBuf, os.Stderr)
	cmd.Stdin = nil

	runErr := cmd.Run()
	if runErr != nil {
		emitDiff(f, fixturePath, fmt.Errorf("capture failed: %w", runErr))
		return false
	}

	m := dumpPathRe.FindSubmatch(stderrBuf.Bytes())
	if len(m) < 2 {
		emitDiff(f, fixturePath, errors.New("no dump-path log line in spike-multiselect stderr"))
		return false
	}
	dumpPath := string(m[1])
	parsedPath := strings.TrimSuffix(dumpPath, ".bin") + ".parsed.json"

	capturedJSON, err := os.ReadFile(parsedPath)
	if err != nil {
		readErr := enrichMissingSidecar(fmt.Errorf("read parsed-json %s: %w", parsedPath, err), stderrBuf.Bytes())
		emitDiff(f, fixturePath, readErr)
		return false
	}

	if record {
		if err := os.WriteFile(fixturePath, capturedJSON, 0o644); err != nil {
			emitDiff(f, fixturePath, fmt.Errorf("write fixture %s: %w", fixturePath, err))
			return false
		}
		fmt.Printf("SNAPSHOT %s recorded\n", f.name)
		return true
	}

	committedJSON, err := os.ReadFile(fixturePath)
	if err != nil {
		emitDiff(f, fixturePath, fmt.Errorf("read fixture %s: %w", fixturePath, err))
		fmt.Fprintf(os.Stderr, "  captured at %s\n", parsedPath)
		return false
	}

	var capturedTree, committedTree any
	if err := json.Unmarshal(capturedJSON, &capturedTree); err != nil {
		emitDiff(f, fixturePath, fmt.Errorf("parse captured json: %w", err))
		fmt.Fprintf(os.Stderr, "  captured at %s\n", parsedPath)
		return false
	}
	if err := json.Unmarshal(committedJSON, &committedTree); err != nil {
		emitDiff(f, fixturePath, fmt.Errorf("parse fixture json: %w", err))
		fmt.Fprintf(os.Stderr, "  captured at %s\n", parsedPath)
		return false
	}

	if !reflect.DeepEqual(capturedTree, committedTree) {
		emitDiff(f, fixturePath, nil)
		fmt.Fprintf(os.Stderr, "  captured at %s\n", parsedPath)
		return false
	}
	fmt.Printf("SNAPSHOT %s match\n", f.name)
	return true
}

// enrichMissingSidecar augments a missing-sidecar read error with the modal
// class spike-multiselect detected, scraped from its stderr. A missing
// .parsed.json with a non-MCP class means the classifier didn't recognise the
// captured UI and the spike's default branch wrote nothing — classifier drift
// masquerading as a content diff (#128). The empty-string class
// (ModalClassUnknown) is rendered as "Unknown". When stderr has no
// modal-class line (spike crashed before logging it), readErr is returned
// unchanged. Only the error message is affected; control flow and exit codes
// are not.
func enrichMissingSidecar(readErr error, stderr []byte) error {
	mc := modalClassRe.FindSubmatch(stderr)
	if mc == nil {
		return readErr
	}
	class := string(mc[1])
	if class == "" {
		class = "Unknown"
	}
	return fmt.Errorf("%w (spike classified modal as %q and wrote no sidecar — classifier drift, not a content diff)", readErr, class)
}

// emitDiff prints the diff verdict to stdout and the operator-facing
// "drift in <path>[: <err>]" line to stderr. err==nil means a clean
// parsed-shape mismatch; non-nil means a capture/read failure degraded to diff.
func emitDiff(f fixture, fixturePath string, err error) {
	fmt.Printf("SNAPSHOT %s diff\n", f.name)
	if err == nil {
		fmt.Fprintf(os.Stderr, "drift in %s\n", fixturePath)
		return
	}
	fmt.Fprintf(os.Stderr, "drift in %s: %v\n", fixturePath, err)
}
