package main

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/pyrycode/tui-driver/pkg/tuidriver"
)

// This file is the pure, unit-tested core of the diagnosis rig (#253 slice A):
// classifyCapture turns one instance's timestamped capture into an
// instanceOutcome, and diagnose maps that outcome to a root-cause hint. Neither
// function does I/O or spawns goroutines; the concurrent capture that produces
// the input lives in main.go.

// Outcome of one spike-permission instance.
const (
	outcomePass  = "pass"  // stdout printed the ^OBSERVED line
	outcomeFlake = "flake" // stderr printed "modal not detected within 30s"
	outcomeOther = "other" // neither marker (spawn failure, unrelated error)
)

// Root-cause hints diagnose can return. The load-bearing one is
// detection-bug-c: it is the only cause that pushes the slice-B fix into
// pkg/tuidriver and makes it security-sensitive under #242. (a)/(b)/ambiguous
// all route to a load-tolerant timing fix in cmd/spike-permission.
const (
	causeContentionA    = "contention-a"              // claude too CPU-starved to render the modal in time
	causePtyStarvationB = "pty-starvation-b"          // modal bytes pooled behind a starved PTY reader
	causeDetectionC     = "detection-bug-c"           // modal drained on-screen but the detector never fired
	causeLoadAmbiguous  = "load-induced-ab-ambiguous" // load-induced, (a) vs (b) not separable from outside
	causeInconclusive   = "inconclusive"              // not a reproduced flake, nothing to diagnose
)

// Stage keys read from spike-permission's stderr log lines
// (cmd/spike-permission/main.go:509,518). prompt-written is when the 30s
// modalDetectLimit clock starts, so it anchors the detect deadline.
const (
	stageProbeStart    = "probe-start"
	stagePromptWritten = "prompt-written"
)

// Timing thresholds, in milliseconds. All three are first-cut heuristics to be
// tuned from the first live make e2e-representative run (see README Open
// questions); they are the knobs, not laws.
const (
	// modalDetectLimitMs mirrors spike-permission's modalDetectLimit
	// (cmd/spike-permission/main.go:112-115) — the internal probe wall the
	// flake races. The harness does not enforce it; it uses it to decide
	// whether a captured anchor arrived before the spike would have given up.
	modalDetectLimitMs int64 = 30_000

	// steadyGapMs: at or below this, the inter-line byte flow reads as steady
	// slow streaming → contention (a), claude itself starved.
	steadyGapMs int64 = 1_500

	// burstGapMs: at or above this, an isolated gap reads as bytes pooling
	// behind a starved reader then bursting → pty-starvation (b).
	burstGapMs int64 = 4_000
)

// modalAnchors is a documented copy of spike-permission's modalLiteralTexts
// (cmd/spike-permission/main.go:150-168). hasModal fires on any of these after
// an ANSI strip, so the harness greps the same three literals in the captured
// PTY stream to answer "did the modal actually render on-screen before the
// timeout?" — the (c)-discriminator. Kept in sync with that source range by
// hand; both cover claude's Bash-tool (CSI-spaced) and Read-tool (literal-space)
// render paths.
var modalAnchors = []string{
	"Esctocancel",           // always CSI-spaced in both render paths — the universal marker
	"Doyouwanttoproceed",    // Bash-tool path: spaces are CSI cursor-forwards, gone after strip
	"Do you want to proceed", // Read-tool path: literal spaces survive the strip
}

// instanceOutcome is the classified result of one capture. Field set is the
// contract the diagnosis table reads; modalSeenAtMs is the load-bearing column.
type instanceOutcome struct {
	outcome       string           // outcomePass | outcomeFlake | outcomeOther
	modalSeenAtMs int64            // earliest elapsed-ms a modal anchor drained, or -1 if never
	stageTimeline map[string]int64 // stage key -> first elapsed-ms it was logged
	maxByteGapMs  int64            // largest inter-line arrival gap in the capture
}

// rootCauseHint is diagnose's verdict for one flake.
type rootCauseHint struct {
	cause    string // one of the cause* constants
	evidence string // one-line, human-readable justification
}

// classifyCapture parses one instance's timestamped capture into an
// instanceOutcome. Each line is "<elapsedMs>\t<text>" as written by the capture
// goroutine (main.go). Anchor matching ANSI-strips first, so a CSI-split render
// (Do\x1b[1Cyou…) collapses to the no-space Doyouwanttoproceed form the same way
// spike-permission's hasModal does. Pure: no I/O, no goroutines.
func classifyCapture(capture []byte) instanceOutcome {
	o := instanceOutcome{outcome: outcomeOther, modalSeenAtMs: -1, stageTimeline: map[string]int64{}}

	sc := bufio.NewScanner(bytes.NewReader(capture))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // PTY lines can be large

	var prevMs int64 = -1
	sawObserved, sawTimeout := false, false

	for sc.Scan() {
		ms, text, ok := parseCaptureLine(sc.Text())
		if !ok {
			continue
		}

		if prevMs >= 0 {
			if gap := ms - prevMs; gap > o.maxByteGapMs {
				o.maxByteGapMs = gap
			}
		}
		prevMs = ms

		if strings.Contains(text, stageProbeStart) {
			if _, seen := o.stageTimeline[stageProbeStart]; !seen {
				o.stageTimeline[stageProbeStart] = ms
			}
		}
		if strings.Contains(text, stagePromptWritten) {
			if _, seen := o.stageTimeline[stagePromptWritten]; !seen {
				o.stageTimeline[stagePromptWritten] = ms
			}
		}

		if strings.HasPrefix(strings.TrimSpace(text), "OBSERVED") {
			sawObserved = true
		}
		if strings.Contains(text, "modal not detected within") {
			sawTimeout = true
		}

		if o.modalSeenAtMs < 0 {
			stripped := tuidriver.StripANSIString(text)
			for _, a := range modalAnchors {
				if strings.Contains(stripped, a) {
					o.modalSeenAtMs = ms
					break
				}
			}
		}
	}

	switch {
	case sawObserved:
		o.outcome = outcomePass
	case sawTimeout:
		o.outcome = outcomeFlake
	default:
		o.outcome = outcomeOther
	}
	return o
}

// parseCaptureLine splits one capture line into its harness-owned elapsed-ms and
// the raw text after it. Lines without the "<int>\t" prefix (blank lines, a
// partial write) are reported not-ok and skipped by the caller.
func parseCaptureLine(line string) (ms int64, text string, ok bool) {
	tab := strings.IndexByte(line, '\t')
	if tab < 0 {
		return 0, "", false
	}
	n, err := strconv.ParseInt(line[:tab], 10, 64)
	if err != nil {
		return 0, "", false
	}
	return n, line[tab+1:], true
}

// diagnose applies the §Diagnosis-rules decision table to one flake. The (c)
// determination is unambiguous and is the only one that changes slice B's
// security posture: because spike-permission's stderr mirror and its rolling
// buffer share one reader goroutine, an anchor captured before the detect
// deadline proves the bytes were both rendered and drained into the spike's
// buffer in time — so a timeout there is a detector defect, not a slow render.
func diagnose(o instanceOutcome) rootCauseHint {
	if o.outcome != outcomeFlake {
		return rootCauseHint{causeInconclusive, "not a reproduced flake; nothing to diagnose"}
	}

	deadline := modalDetectLimitMs
	if pw, ok := o.stageTimeline[stagePromptWritten]; ok {
		deadline = pw + modalDetectLimitMs
	}

	if o.modalSeenAtMs >= 0 && o.modalSeenAtMs < deadline {
		return rootCauseHint{causeDetectionC, fmt.Sprintf(
			"modal anchor drained on-screen at %dms, before the %dms detect deadline, yet waitForModal timed out — the detector failed, not the render",
			o.modalSeenAtMs, deadline)}
	}

	switch {
	case o.maxByteGapMs >= burstGapMs:
		return rootCauseHint{causePtyStarvationB, fmt.Sprintf(
			"no anchor drained before the deadline and an isolated %dms byte-gap — bytes pooled then burst, PTY reader starvation",
			o.maxByteGapMs)}
	case o.maxByteGapMs <= steadyGapMs:
		return rootCauseHint{causeContentionA, fmt.Sprintf(
			"no anchor drained before the deadline, byte flow steady (max gap %dms) — claude itself too CPU-starved to render the modal in time",
			o.maxByteGapMs)}
	default:
		return rootCauseHint{causeLoadAmbiguous, fmt.Sprintf(
			"no anchor drained before the deadline, gaps mixed (max %dms, between the steady and burst thresholds) — load-induced, but (a) contention vs (b) reader-starvation not separable from a black-box capture",
			o.maxByteGapMs)}
	}
}

// sharedWith185 maps a root-cause hint to the shared-with-#185 determination
// that selects slice B's branch and security posture. This is the AC deliverable
// the operator's live run records; the final call is theirs, this states what
// the signals imply.
func sharedWith185(cause string) string {
	switch cause {
	case causeDetectionC:
		return "distinct from #185 — detection defect in pkg/tuidriver, security-sensitive under #242 (preserve the region-scoped shape-gate)"
	case causeContentionA, causePtyStarvationB, causeLoadAmbiguous:
		return "shared family with #185 — load-induced, fix in cmd/spike-permission, not security-sensitive"
	default:
		return "undetermined — insufficient signal to map"
	}
}
