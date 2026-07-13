package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// apiKeyEnv is the Anthropic API-key variable. Its presence in the child
// environment flips claude from the ambient subscription login to metered
// billing — the exact outcome this design avoids — so childEnv REMOVES it.
const apiKeyEnv = "ANTHROPIC_API_KEY"

// useStreamJSONEnv is pyry's legacy-path selector. When set to "1", `pyry
// agent-run` routes through the metered stream-json print-mode subprocess
// instead of the subscription-billed interactive path. childEnv REMOVES it so
// the child always takes the default (subscription) path — the billing
// invariant, alongside apiKeyEnv.
const useStreamJSONEnv = "PYRY_USE_STREAMJSON"

// allowedToolsMinimal is the fixed --allowed-tools value. `pyry agent-run`
// requires a non-empty allow-list (it is the load-bearing tool gate), but the
// judge is a pure reader: the system prompt forbids tool use, and a small
// --max-turns backs that up. A single read-only tool satisfies pyry's
// non-empty requirement without granting anything the judge is meant to use.
const allowedToolsMinimal = "Read"

// outputFormatStreamJSON is the only output format `pyry agent-run` accepts.
// The model's final text is the `result` field of the trailing
// `type:"result"` line; parseResultText reads exactly that.
const outputFormatStreamJSON = "stream-json"

// labelMaxTurns bounds the judge's turns. It must be > 1: `pyry agent-run`
// counts logical assistant turns and fires the max-turns stop at the cap even
// on the end-of-turn entry, so a budget of 1 would mark a clean single-turn
// answer as an error. A read-and-label reply is one logical turn; a small
// slack above that lets it complete cleanly while still bounding a runaway.
const labelMaxTurns = 4

// batchTimeout bounds a single batch's `pyry agent-run` call. An interactive
// claude spawn costs ~10-20s before it answers, so this is generous; its only
// job is to catch a truly hung call. A timed-out batch surfaces as a failed
// attempt, which parks and re-attempts on the next resume run.
const batchTimeout = 8 * time.Minute

// runner turns a batch prompt into the judge's raw reply text. The one-method
// seam lets labelBatch stay identical across the production pyryRunner and the
// test fake.
type runner interface {
	run(ctx context.Context, prompt string) (string, error)
}

// pyryRunner is the production runner: it drives one batch through a fresh
// `pyry agent-run`, which spawns interactive claude on the subscription login
// (no metered API spend) and ends when the turn completes. Each batch is
// judged cold — the point of an independent judge.
type pyryRunner struct {
	pyryPath         string        // -pyry; the test injection seam
	model            string        // -model; passed via --model
	effort           string        // -effort; passed via --effort
	workdir          string        // -workdir; passed via --workdir (must exist; pyry pre-marks it trusted)
	systemPromptPath string        // temp system-prompt file, written once at labeler init
	maxTurns         int           // --max-turns budget
	timeout          time.Duration // per-batch deadline
}

// run writes the batch prompt to a temp file, invokes `pyry agent-run` with a
// child environment scrubbed of the metered-billing variables, and returns the
// judge's reply text parsed from the trailing result line.
//
// Grid safety: pyry's stream-json stdout echoes the delivered user prompt (the
// grids) verbatim as a `type:"user"` line. That stdout is NEVER returned to the
// caller — only the parsed `result` text (the model's reply, grid-free) on
// success, or a grid-free error otherwise. A park record can therefore never
// carry grid content. pyry's stderr is grid-free by contract (its stream-json
// layer must not log entry content), so it is forwarded to the operator to make
// an auth 401 or spawn failure visible during a multi-day run.
func (r *pyryRunner) run(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	promptFile, cleanup, err := writeTempFile("corpus-label-prompt-*.txt", prompt)
	if err != nil {
		return "", fmt.Errorf("writing prompt file: %w", err)
	}
	defer cleanup()

	cmd := exec.CommandContext(ctx, r.pyryPath, buildAgentRunArgs(r, promptFile)...)
	cmd.Env = childEnv(os.Environ())
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Never surface stdout here — it may echo the grids. Report the error
		// only; the batch parks with a grid-free note.
		return "", fmt.Errorf("pyry agent-run: %w", err)
	}
	text, err := parseResultText(out.String())
	if err != nil {
		return "", err // parseResultText returns grid-free errors only
	}
	return text, nil
}

// buildAgentRunArgs assembles the `pyry agent-run` flag list, pinning the
// invariants: the subscription-path output format, the model, the turn budget,
// and the fixed minimal allow-list. The workdir, effort, and both prompt files
// come from the runner.
func buildAgentRunArgs(r *pyryRunner, promptFile string) []string {
	return []string{
		"agent-run",
		"--prompt-file", promptFile,
		"--system-prompt-file", r.systemPromptPath,
		"--allowed-tools", allowedToolsMinimal,
		"--max-turns", strconv.Itoa(r.maxTurns),
		"--effort", r.effort,
		"--model", r.model,
		"--workdir", r.workdir,
		"--output-format", outputFormatStreamJSON,
	}
}

// resultLine is the subset of pyry's `type:"result"` trailer that corpus-label
// reads: the type discriminator, the error flags, and the model's final text.
type resultLine struct {
	Type           string `json:"type"`
	Subtype        string `json:"subtype"`
	IsError        bool   `json:"is_error"`
	Result         string `json:"result"`
	TerminalReason string `json:"terminal_reason"`
}

// parseResultText returns the `result` field of the last `type:"result"` line
// in pyry's stream-json output. Non-result lines (init, per-turn events, and
// the user-prompt echo that carries the grids) are ignored, so grid content
// never flows through. Errors are grid-free: an error-typed trailer or a
// missing result line returns an error with no stdout attached.
func parseResultText(stdout string) (string, error) {
	var last *resultLine
	for _, ln := range strings.Split(stdout, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || ln[0] != '{' {
			continue
		}
		var rl resultLine
		if err := json.Unmarshal([]byte(ln), &rl); err != nil {
			continue // a non-JSON or partial line is not the trailer
		}
		if rl.Type == "result" {
			v := rl
			last = &v
		}
	}
	if last == nil {
		return "", fmt.Errorf("no result line in pyry agent-run output")
	}
	if last.IsError {
		return "", fmt.Errorf("pyry agent-run ended in error: subtype=%q terminal_reason=%q", last.Subtype, last.TerminalReason)
	}
	return last.Result, nil
}

// childEnv returns parent with the two metered-billing variables removed and
// everything else — crucially CLAUDE_CODE_OAUTH_TOKEN, the durable
// subscription auth — preserved. Stripping ANTHROPIC_API_KEY keeps claude on
// the subscription login; stripping PYRY_USE_STREAMJSON keeps pyry on its
// default subscription path rather than the metered print-mode fallback.
// Replaces the old single-variable scrubEnv.
func childEnv(parent []string) []string {
	out := make([]string, 0, len(parent))
	for _, kv := range parent {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if key == apiKeyEnv || key == useStreamJSONEnv {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// systemPromptText steers the judge to a pure text answer: no tools, no file
// reads, just the JSON array the batch prompt asks for. It backs the taxonomy
// prompt in buildPrompt (which is unchanged) and the minimal allow-list.
const systemPromptText = `You are an independent judge classifying terminal-screen captures.
Use no tools. Do not read files. Do not run commands.
Respond with ONLY the JSON array the user's message asks for, and nothing else.
`

// writeSystemPrompt writes systemPromptText to a temp file for --system-prompt-file
// and returns its path plus a cleanup func. Written once at labeler init and
// reused across every batch.
func writeSystemPrompt() (path string, cleanup func(), err error) {
	return writeTempFile("corpus-label-system-*.txt", systemPromptText)
}

// writeTempFile writes content to a new temp file matching pattern and returns
// its path plus a cleanup func that removes it. The prompt temp file carries
// grids; it lives only in the OS temp dir for the duration of the call and is
// removed after, never touching -out or the vault.
func writeTempFile(pattern, content string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", nil, err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", nil, err
	}
	name := f.Name()
	return name, func() { os.Remove(name) }, nil
}
