// e2e-snapshot-check re-derives each committed fixture under
// pkg/tuidriver/testdata/ by running spike-multiselect with the appropriate
// -trigger and -settle flags, then byte-compares the resulting /tmp dump to
// the committed file. Exits 0 iff every fixture matched.
//
// Read-only invariant (per ticket #35): this binary never writes to
// pkg/tuidriver/testdata/* — re-recording is a maintainer step run
// separately via spike-multiselect.
//
// Stdout protocol (one line per fixture, in fixture-table order):
//
//	SNAPSHOT picker match|diff
//	SNAPSHOT mcp match|diff
//	SNAPSHOT agents match|diff
//
// Stderr carries operator-facing diagnostics (mirrored spike-multiselect
// output, per-fixture "drift in <path>" lines on diff).
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	testdataDir := flag.String("testdata-dir", "./pkg/tuidriver/testdata", "directory containing committed *-snapshot.bin fixtures")
	spikeTimeout := flag.Duration("spike-timeout", 60*time.Second, "per-fixture spike-multiselect wall budget")
	flag.Parse()

	fixtures := []fixture{
		{name: "picker", trigger: "/", settle: 0},
		{name: "mcp", trigger: "/mcp\r", settle: 5 * time.Second},
		{name: "agents", trigger: "/agents\r", settle: 0},
	}

	allOK := true
	for _, f := range fixtures {
		ok := runFixture(*binDir, *testdataDir, *spikeTimeout, f)
		if !ok {
			allOK = false
		}
	}
	if !allOK {
		os.Exit(1)
	}
}

func runFixture(binDir, testdataDir string, spikeTimeout time.Duration, f fixture) bool {
	fixturePath := filepath.Join(testdataDir, f.name+"-snapshot.bin")

	ctx, cancel := context.WithTimeout(context.Background(), spikeTimeout)
	defer cancel()

	args := []string{"-trust-folder=accept", "-trigger=" + f.trigger}
	if f.settle > 0 {
		args = append(args, "-settle="+f.settle.String())
	}
	binPath := filepath.Join(binDir, "spike-multiselect")

	cmd := exec.CommandContext(ctx, binPath, args...)
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

	captured, err := os.ReadFile(dumpPath)
	if err != nil {
		emitDiff(f, fixturePath, fmt.Errorf("read dump %s: %w", dumpPath, err))
		return false
	}
	committed, err := os.ReadFile(fixturePath)
	if err != nil {
		emitDiff(f, fixturePath, fmt.Errorf("read fixture %s: %w", fixturePath, err))
		return false
	}

	if !bytes.Equal(normalize(captured), normalize(committed)) {
		emitDiff(f, fixturePath, nil)
		return false
	}
	fmt.Printf("SNAPSHOT %s match\n", f.name)
	return true
}

// emitDiff prints the diff verdict to stdout and the operator-facing
// "drift in <path>[: <err>]" line to stderr. err==nil means a clean
// byte-mismatch; non-nil means a capture/read failure degraded to diff.
func emitDiff(f fixture, fixturePath string, err error) {
	fmt.Printf("SNAPSHOT %s diff\n", f.name)
	if err == nil {
		fmt.Fprintf(os.Stderr, "drift in %s\n", fixturePath)
		return
	}
	fmt.Fprintf(os.Stderr, "drift in %s: %v\n", fixturePath, err)
}
