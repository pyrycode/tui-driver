// Probe: observe how `claude` encodes a non-ASCII working directory into the
// `~/.claude/projects/<name>` directory it writes its per-session JSONL under.
//
// tuidriver.EncodeCwd derives that directory name by mapping every BYTE outside
// [a-zA-Z0-9] to one '-' — so a multi-byte char like `ö` (2 UTF-8 bytes) yields
// two hyphens. Whether that matches claude is unverified (the empirical
// derivation behind EncodeCwd exercised only ASCII specials). This probe
// observes the golden value from REAL claude so #207 can pin a unit test
// against claude's actual filesystem behaviour rather than an assumption.
//
// Sequence:
//
//	create a temp cwd whose LEAF component is "Työ😀" (one BMP multi-byte char
//	  `ö` + one astral char `😀` — discriminates all three candidate rules)
//	→ spawn `claude --session-id <fresh-uuid>` with cmd.Dir = that cwd
//	→ wait for idle → accept the (always-present, brand-new-cwd) trust modal
//	→ send one trivial prompt (only to make claude create the session JSONL,
//	  which it defers until first input lands — spike-one-turn finding #9)
//	→ discover the projects-dir INDEPENDENTLY of EncodeCwd by globbing
//	  ~/.claude/projects/*/<session-id>.jsonl and taking the parent dir's base
//	→ derive which encoding rule (per-byte / per-UTF-16 code unit / per-rune)
//	  produced it via a leaf-suffix match, and record the raw name byte-for-byte
//
// It is a recording rig, not a library regression test: it ships GREEN whenever
// it makes an observation — it never asserts the observation agrees with
// EncodeCwd (a mismatch is the expected, useful result). It exits non-zero only
// when no observation could be made (spawn failed, never idle, JSONL never
// appeared). This file is spike-quality: single binary, no public API. See
// cmd/probe-cwd-encoding/README.md for the empirical log. Ticket #206.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
	"github.com/pyrycode/tui-driver/pkg/tuidriver"
)

const (
	// sessionFileWait bounds the glob-poll for claude's session JSONL to
	// appear. Interactive `claude --session-id` defers JSONL creation until it
	// processes the first input (spike-one-turn finding #9), and that work is
	// local-process-bound so it slows under concurrent-suite load (#185). 30s
	// matches cmd/spike-one-turn/main.go:51 and cmd/probe-first-prompt-hang/
	// main.go:41 — same deferred-JSONL-creation reason, cited not re-derived —
	// and stays inside the e2e-runner's 60s per-check budget.
	sessionFileWait = 30 * time.Second

	// wallTimeout bounds the whole run below the runner's 60s cap while leaving
	// room for the 30s glob-poll plus the idle/trust/prompt lead-in.
	wallTimeout = 50 * time.Second

	shutdownGrace = 3 * time.Second

	// settleWindow is how long the PTY must stay quiet (no new bytes) before a
	// target state counts as "settled". It re-anchors the throwaway-prompt send
	// off the bare-❯ false idle that appears ~0.25s into startup while claude is
	// still rendering (#251/#263): requiring quiescence waits past that render
	// burst so the keystroke lands on a genuinely ready claude. Must exceed the
	// inter-render gap during startup/trust transitions yet stay far under
	// sessionFileWait/wallTimeout. This is the tuning lever the operator adjusts
	// across the #263 live `make e2e` runs (same posture as the e2e-runner spike
	// timeouts); document the value that reaches 10-in-a-row.
	settleWindow = 1 * time.Second

	// trustModalWait bounds phase 1's wait for the (normally guaranteed) trust
	// modal to appear and settle before falling back to the quiescent-idle path,
	// so a rare auto-trusted cwd does not hang to wallTimeout. Under wallTimeout
	// with room for the post-trust settle plus the 30s sessionFileWait.
	trustModalWait = 15 * time.Second

	// promptText is any trivial input — we only need claude to create the
	// session JSONL (which materialises the projects-dir), not to finish a turn.
	promptText = "hi\r"

	// nonASCIILeaf is the working-directory leaf component. `ö` (U+00F6) is a
	// BMP multi-byte char (2 UTF-8 bytes / 1 UTF-16 unit / 1 rune); `😀`
	// (U+1F600) is astral (4 UTF-8 bytes / 2 UTF-16 units / 1 rune). Together
	// they make the per-byte, per-UTF-16 and per-rune encodings all distinct,
	// so a single run picks exactly one rule (see the README table). A literal
	// in a .go source file — no shell interpolation, safe on APFS/ext4/tmpfs.
	nonASCIILeaf = "Työ😀"
)

func main() {
	trustFolderFlag := flag.String("trust-folder", "accept",
		"policy when claude's trust-folder dialog appears: 'fail' (return a clear error) or 'accept' (auto-trust the fresh cwd, then proceed)")
	flag.Parse()

	if *trustFolderFlag != "fail" && *trustFolderFlag != "accept" {
		fmt.Fprintf(os.Stderr, "invalid -trust-folder value %q (want 'fail' or 'accept')\n", *trustFolderFlag)
		os.Exit(2)
	}

	if err := run(*trustFolderFlag); err != nil {
		fmt.Fprintf(os.Stderr, "probe failed: %v\n", err)
		os.Exit(1)
	}
}

func run(trustFolderPolicy string) error {
	startedAt := time.Now()
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)

	// Timestamped recording dir under os.TempDir() — a durable artifact even on
	// a failed run. Logged to stderr (the runner mirrors it) in the same
	// `probe outDir=<path>` shape probe-first-prompt-hang uses.
	tsTag := startedAt.Format("20060102-150405")
	outDir := filepath.Join(os.TempDir(), "probe-cwd-encoding-"+tsTag)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir recording dir: %w", err)
	}
	logger.Printf("probe outDir=%s", outDir)

	// Fresh session ID — never reused. Makes <session-id>.jsonl globally unique
	// across every projects-dir, so the discovery glob is unambiguous (no
	// stale-match race — the failure mode spike-one-turn finding #9 documents).
	u, err := uuid.NewRandom()
	if err != nil {
		return fmt.Errorf("generate session id: %w", err)
	}
	sessionID := u.String()

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}

	// Non-ASCII working directory: a temp root plus a leaf carrying the
	// multi-byte chars. claude launches here via cmd.Dir (Spawn hands cmd
	// straight to StartPTY, so cmd.Dir passes through unchanged).
	root, err := os.MkdirTemp("", "probe-cwd-encoding-*")
	if err != nil {
		return fmt.Errorf("mkdir temp root: %w", err)
	}
	defer os.RemoveAll(root) // tidy the cwd; leave ~/.claude/projects/<dir> as evidence.
	cwd := filepath.Join(root, nonASCIILeaf)
	if err := os.Mkdir(cwd, 0o755); err != nil {
		return fmt.Errorf("mkdir non-ascii cwd %q: %w", cwd, err)
	}
	logger.Printf("session-id=%s cwd=%s", sessionID, cwd)

	rootCtx, cancelCause := context.WithCancelCause(context.Background())
	defer cancelCause(errors.New("run: returning"))

	// Wall timeout — graceful cancel, not a process kill; every bounded wait
	// below hangs off rootCtx.
	wallTimer := time.AfterFunc(wallTimeout, func() {
		cancelCause(fmt.Errorf("wall timeout (%s) reached", wallTimeout))
	})
	defer wallTimer.Stop()

	cmd := exec.Command("claude", "--session-id", sessionID)
	cmd.Dir = cwd
	tuidriver.EnsureClaudeEnv(cmd)

	session, err := tuidriver.Spawn(cmd, tuidriver.SpawnOpts{
		MirrorStderr:  true,
		ShutdownGrace: shutdownGrace,
	})
	if err != nil {
		return fmt.Errorf("Spawn: %w", err)
	}
	defer func() {
		logger.Printf("shutdown-signalled")
		_ = session.Close()
		cancelCause(errors.New("shutdown"))
	}()

	// Re-anchor (#263): replace the two edge-triggered IsIdle/HasTrustModal
	// checks with a level-triggered "settled" gate — a target state must hold
	// AND the PTY must have been quiet for settleWindow. The bare ❯ that appears
	// ~0.25s into startup is a transient produced WHILE claude is still rendering
	// (#251 false idle); requiring quiescence waits past that render burst so the
	// throwaway prompt lands on a genuinely ready claude. Prefer a semantic
	// anchor (the trust modal) where one exists; fall back to bare quiescence
	// only where none does.
	readinessSignal := "trust-modal-settled"

	// Phase 1: wait for the trust modal, settled. A brand-new temp cwd is never
	// trusted, so claude ALWAYS shows this — its appearance means the startup
	// render finished and an interactive dialog is up (a stronger signal than a
	// bare idle). Bounded by trustModalWait so a rare auto-trusted cwd falls back
	// to the quiescent-idle path instead of hanging to the wall timeout.
	trustCtx, cancelTrust := context.WithTimeout(rootCtx, trustModalWait)
	trustErr := tuidriver.WaitUntil(trustCtx, settledPredicate(session, settleWindow, tuidriver.HasTrustModal))
	cancelTrust()

	trustModalSeen := trustErr == nil
	switch {
	case trustModalSeen:
		logger.Printf("trust-modal-settled elapsed=%s quiet=%s",
			time.Since(startedAt).Round(time.Millisecond), settleWindow)
	case rootCtx.Err() != nil:
		// The wall timeout / shutdown collapsed the whole run — a real failure,
		// not the auto-trust fallback. Surface rootCtx's cause.
		return fmt.Errorf("wait settled trust modal: %w", context.Cause(rootCtx))
	default:
		// trustModalWait elapsed with no modal — an auto-trusted cwd. Fall back
		// to the quiescent-idle send path (weaker: no semantic anchor).
		readinessSignal = "quiescent-idle-fallback"
		logger.Printf("trust-modal-absent-within=%s falling-back-to-quiescent-idle", trustModalWait)
	}

	// Phase 2: answer trust per policy — reached only once the modal is genuinely
	// settled, so -trust-folder=fail no longer races the false idle.
	if trustModalSeen {
		if trustFolderPolicy == "fail" {
			return fmt.Errorf("claude shows the trust-folder dialog for the fresh cwd — pass -trust-folder=accept")
		}
		if err := session.AcceptTrust(); err != nil {
			return fmt.Errorf("write trust-accept keystroke: %w", err)
		}
		logger.Printf("trust-accepted elapsed=%s", time.Since(startedAt).Round(time.Millisecond))
	}

	// Phase 3: wait for a settled post-trust idle, then send. This is the actual
	// send-readiness gate the root cause names ("after trust-accept"): ❯ present,
	// no trust modal, and the PTY quiet for settleWindow.
	if err := tuidriver.WaitUntil(rootCtx, settledPredicate(session, settleWindow, func(snap []byte) bool {
		return !tuidriver.HasTrustModal(snap) && tuidriver.IsIdle(snap)
	})); err != nil {
		return fmt.Errorf("wait settled idle before prompt: %w", err)
	}
	elapsedToReady := time.Since(startedAt)
	logger.Printf("post-trust-settled-idle elapsed=%s quiet=%s",
		elapsedToReady.Round(time.Millisecond), settleWindow)

	// Send one trivial prompt — the only reason is to make claude create the
	// session JSONL (deferred until first input lands). Once the file exists on
	// disk, the projects-dir exists and is named — that is the whole
	// observation. No JSONL tail, no end-turn detection, no watchdog.
	if err := session.SendKeys(promptText); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	logger.Printf("prompt-written elapsed=%s", time.Since(startedAt).Round(time.Millisecond))

	// Discover claude's projects-dir INDEPENDENTLY of EncodeCwd. This never
	// calls EncodeCwd / SessionJSONLPath / WaitForSessionJSONL — those derive
	// the path via EncodeCwd, so they could only ever confirm the current
	// transform, never observe a mismatch.
	observedDir, err := discoverProjectsDir(rootCtx, home, sessionID)
	if err != nil {
		return err
	}
	logger.Printf("observed-projects-dir=%s", observedDir)

	// Record the observation + derived rule. This ALWAYS ships green once
	// observedDir is known — including when it contradicts EncodeCwd (the
	// expected, useful result) and when no candidate rule matches (unknown).
	report := deriveObservation(sessionID, cwd, observedDir)
	report.readinessSignal = readinessSignal
	report.elapsedToReady = elapsedToReady
	writeObservation(logger, outDir, report)
	logger.Printf("observation recorded (elapsed %s)", time.Since(startedAt).Round(time.Millisecond))
	return nil
}

// settledPredicate returns a WaitUntil predicate that holds once want(snapshot)
// is true AND the PTY has been quiet for window — i.e. claude finished the
// render burst that produced this state, rather than sitting on the transient
// false idle mid-render (#251/#263). window == 0 degrades to a bare want()
// check. LastAppendAt is read AFTER the snapshot so any append that shaped the
// snapshot is reflected in the quiescence clock: the conservative order can only
// under-report quiet (making us poll once more), never over-report it.
func settledPredicate(s *tuidriver.Session, window time.Duration, want func([]byte) bool) func() bool {
	return func() bool {
		snap := s.Snapshot()
		return isSettled(want(snap), time.Since(s.LastAppendAt()), window)
	}
}

// isSettled is settledPredicate's pure, claude-free-testable core: a settled
// gate holds when the target state is present (want) AND the PTY has been quiet
// for at least window (sinceLastAppend >= window). window == 0 makes any
// non-negative quiet duration pass, degrading to a bare want() check.
func isSettled(want bool, sinceLastAppend, window time.Duration) bool {
	if !want {
		return false
	}
	return sinceLastAppend >= window
}

// discoverProjectsDir polls ~/.claude/projects/*/<sessionID>.jsonl until a
// match appears or sessionFileWait elapses, then returns the matched file's
// parent-directory base name — claude's projects-dir, byte-for-byte. An empty
// glob at the deadline is the correct "could not observe" signal → error → the
// check goes red.
func discoverProjectsDir(ctx context.Context, home, sessionID string) (string, error) {
	pattern := filepath.Join(home, ".claude", "projects", "*", sessionID+".jsonl")
	pollCtx, cancel := context.WithTimeout(ctx, sessionFileWait)
	defer cancel()
	ticker := time.NewTicker(tuidriver.DefaultPollInterval)
	defer ticker.Stop()
	for {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return "", fmt.Errorf("glob %s: %w", pattern, err)
		}
		if len(matches) > 0 {
			if len(matches) > 1 {
				// Should not happen with a fresh UUID; note it and use [0].
				fmt.Fprintf(os.Stderr, "probe-cwd-encoding: WARNING glob matched %d files for session %s; using %s\n",
					len(matches), sessionID, matches[0])
			}
			return filepath.Base(filepath.Dir(matches[0])), nil
		}
		select {
		case <-pollCtx.Done():
			return "", fmt.Errorf("projects-dir for session %s not found under ~/.claude/projects within %s: %w",
				sessionID, sessionFileWait, context.Cause(pollCtx))
		case <-ticker.C:
		}
	}
}

// observation is the recorded finding for one run.
type observation struct {
	sessionID    string
	cwd          string
	canonical    string
	observedDir  string
	perByte      string // leaf encoded per the current EncodeCwd (per-UTF-8-byte) rule
	perUTF16     string // leaf encoded per JS /[^a-zA-Z0-9]/g (per-UTF-16-code-unit) rule
	perRune      string // leaf encoded per-Unicode-character rule
	derivedRule  string // one of the ruleCandidate names, or "unknown"
	matchedRules []string
	wholePath    string // whole-path cross-check: matching rule name, or "none"
	encodeCwd    string // EncodeCwd(cwd) — informational cross-check only
	matches      bool   // encodeCwd == observedDir

	// Readiness-timing record for the re-anchor (#263), the per-run evidence the
	// operator diffs across the 10 live make-e2e runs (AC2/AC3).
	readinessSignal string        // "trust-modal-settled" or "quiescent-idle-fallback"
	elapsedToReady  time.Duration // run start → settled send-readiness gate
}

// deriveObservation computes the encoding-rule derivation from the compile-time
// leaf and the observed directory. It depends only on the known leaf via a
// leaf-suffix match — immune to any ASCII-prefix canonicalisation surprise.
func deriveObservation(sessionID, cwd, observedDir string) observation {
	// EvalSymlinks resolves the temp dir's parent symlink (macOS /var → /private
	// /var); it does not case-fold on macOS, but a self-created temp dir has no
	// casing ambiguity. Used only for the optional whole-path cross-check.
	canonical, evalErr := filepath.EvalSymlinks(cwd)
	if evalErr != nil {
		canonical = cwd
	}

	rule, matched := deriveRule(observedDir, nonASCIILeaf)

	// Optional whole-path cross-check: does observedDir equal a candidate's
	// full-string encoding of the canonical path? Reported as a bonus; the
	// leaf-suffix match above is authoritative for rule derivation.
	wholePath := "none"
	for _, c := range candidates {
		if c.encode(canonical) == observedDir {
			wholePath = c.name
			break
		}
	}

	encodeCwd := tuidriver.EncodeCwd(cwd)
	return observation{
		sessionID:    sessionID,
		cwd:          cwd,
		canonical:    canonical,
		observedDir:  observedDir,
		perByte:      perByte(nonASCIILeaf),
		perUTF16:     perUTF16(nonASCIILeaf),
		perRune:      perRune(nonASCIILeaf),
		derivedRule:  rule,
		matchedRules: matched,
		wholePath:    wholePath,
		encodeCwd:    encodeCwd,
		matches:      encodeCwd == observedDir,
	}
}

// ruleCandidate is one candidate cwd-encoding rule.
type ruleCandidate struct {
	name   string
	encode func(string) string
}

// candidates are ordered most-specific first. per-byte differs from the other
// two whenever any multi-byte char is present; per-UTF-16 and per-rune diverge
// only on astral chars (both give one hyphen for a BMP char). With the
// astral-inclusive leaf all three are distinct, so exactly one matches.
var candidates = []ruleCandidate{
	{"per-unicode-character", perRune},
	{"per-utf16-code-unit", perUTF16},
	{"per-byte", perByte},
}

// deriveRule returns the encoding rule whose leaf encoding is a suffix of
// observedDir, and the full list of matching candidates. claude's encoding is
// position-preserving and the '/' separating the leaf from its parent encodes
// to '-', so observedDir ends with "-" + <candidate leaf encoding> for the
// winning rule. Returns ("unknown", nil) if none match — claude did something
// unmodelled (e.g. NFC/NFD normalisation, case-folding); the raw observedDir is
// still recorded and the probe still exits 0.
func deriveRule(observedDir, leaf string) (rule string, matched []string) {
	for _, c := range candidates {
		if strings.HasSuffix(observedDir, "-"+c.encode(leaf)) {
			matched = append(matched, c.name)
		}
	}
	if len(matched) == 0 {
		return "unknown", nil
	}
	return matched[0], matched
}

// perByte maps every byte outside [a-zA-Z0-9] to one '-' (one hyphen per UTF-8
// byte) — the current EncodeCwd rule. Kept as a local reference implementation
// so the derivation is transparent; not imported from EncodeCwd.
func perByte(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if isASCIIAlnum(s[i]) {
			b.WriteByte(s[i])
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// perRune maps every non-ASCII-alnum rune to exactly one '-' (one hyphen per
// Unicode character).
func perRune(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 128 && isASCIIAlnum(byte(r)) {
			b.WriteByte(byte(r))
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// perUTF16 maps every non-ASCII-alnum rune to one '-' per UTF-16 code unit (1
// for BMP, 2 for astral). This is JS `.replace(/[^a-zA-Z0-9]/g,'-')` semantics
// — the hypothesised claude rule.
func perUTF16(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 128 && isASCIIAlnum(byte(r)) {
			b.WriteByte(byte(r))
			continue
		}
		for range utf16.Encode([]rune{r}) {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func isASCIIAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// writeObservation prints the OBSERVED: block to stdout (the runner's
// observedSuccess = ^OBSERVED gate) and mirrors it into observation.log under
// outDir. A leaf-suffix ambiguity (only possible with a BMP-only fallback leaf,
// where per-utf16 and per-unicode-character coincide) is warned to stderr.
func writeObservation(logger *log.Logger, outDir string, o observation) {
	if len(o.matchedRules) > 1 {
		fmt.Fprintf(os.Stderr, "probe-cwd-encoding: WARNING %d rules match the leaf suffix (%s); reporting %s — the leaf does not disambiguate them\n",
			len(o.matchedRules), strings.Join(o.matchedRules, ","), o.derivedRule)
	}

	lines := []string{
		fmt.Sprintf("OBSERVED: probe-cwd-encoding session=%s cwd=%s canonical=%s", o.sessionID, o.cwd, o.canonical),
		fmt.Sprintf("OBSERVED: observed_projects_dir=%s", o.observedDir),
		fmt.Sprintf("OBSERVED: leaf=%s per_byte=%s per_utf16=%s per_rune=%s", nonASCIILeaf, o.perByte, o.perUTF16, o.perRune),
		fmt.Sprintf("OBSERVED: derived_rule=%s whole_path_cross_check=%s", o.derivedRule, o.wholePath),
		fmt.Sprintf("OBSERVED: encode_cwd_current=%s matches_observed=%t", o.encodeCwd, o.matches),
		fmt.Sprintf("OBSERVED: readiness_signal=%s elapsed_to_ready=%s settle_window=%s",
			o.readinessSignal, o.elapsedToReady.Round(time.Millisecond), settleWindow),
	}

	var sb strings.Builder
	for _, ln := range lines {
		fmt.Println(ln)
		sb.WriteString(ln)
		sb.WriteByte('\n')
	}
	logPath := filepath.Join(outDir, "observation.log")
	if err := os.WriteFile(logPath, []byte(sb.String()), 0o644); err != nil {
		logger.Printf("WARNING write observation.log: %v", err)
		return
	}
	logger.Printf("observation.log=%s", logPath)
}
