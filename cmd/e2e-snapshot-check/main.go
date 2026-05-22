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
//	SNAPSHOT picker match|diff|recorded
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

type fixture struct {
	name    string // "picker" | "mcp" | "agents"
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
		{name: "picker", trigger: "/", settle: 0},
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
		emitDiff(f, fixturePath, fmt.Errorf("read parsed-json %s: %w", parsedPath, err))
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
