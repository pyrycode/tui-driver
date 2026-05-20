# tui-driver

Go library for driving interactive `claude` CLI sessions via PTY.

## Scope

**Owns:** PTY allocation, rolling byte buffer with quiet-time tracking, ANSI/OSC stripping, vt10x-backed grid rendering, state detection, modal-class detection + parsers, MCP failure detection, session lifecycle, two-arm watchdog (PTY-quiet + spinner-freeze).

**Does not own:** JSONL parsing, ACP protocol, agent-stage logic, dispatcher coordination, cost telemetry.

Consumer pairs `tui-driver` output (live state) with the target binary's session JSONL log (structured content). This separation is intentional — JSONL is missing exactly the state signals (multiselects, thinking spinner, idle prompt) that the TUI shows clearly, and the TUI is missing exactly the structured content that the JSONL records cleanly.

## Status

**Library substantively complete (2026-05-18).** Six spike binaries under `cmd/` validate the primitives end-to-end and serve as regression-detection harnesses. `pkg/tuidriver/` exports the consumer-facing API. ~80 unit tests + 3 PTY-snapshot regression fixtures.

Pending: `ParseAskUserQuestion` (blocked by first-prompt-blocks-on-tool-readiness bug); pyry-acp consumer integration.

## Running the e2e harness

```sh
make e2e                            # local: inherit operator's Claude config (Max-subscription path)
make e2e MODEL=haiku EFFORT=low     # CI: pin --model haiku --effort low to cap metered-API spend
```

Cost differential: Opus high (the default operator config) costs ~$0.50–$2 per spike × 7 spikes + probe ≈ **$3–$15 per CI run**. Haiku low runs ~$0.20–$0.70 per run — roughly **18–90× cheaper** at parity coverage. The spikes test PTY / JSONL / modal behaviour, not reasoning quality, so Haiku low is fine. The `MODEL=` / `EFFORT=` Make variables ride the `TUIDRIVER_CLAUDE_MODEL` / `TUIDRIVER_CLAUDE_EFFORT` env vars through `EnsureClaudeEnv`; unset = inherited operator config. See `docs/knowledge/features/e2e-harness.md` for the full doc.

## Public API

### Process lifecycle

```go
type Session struct {
    Buffer *Buffer  // rolling buffer the reader goroutine writes to
    PTY    *os.File // PTY master; consumers Write keystrokes here
}

func Spawn(cmd *exec.Cmd, opts SpawnOpts) (*Session, error)
func (*Session) Write(p []byte) (int, error)
func (*Session) Wait() error
func (*Session) Close() error

func EnsureClaudeEnv(cmd *exec.Cmd) *exec.Cmd  // sets TERM=xterm-256color
func StartPTY(cmd *exec.Cmd) (*os.File, error) // pty.Start + default Setsize
```

### Buffer + state detection

```go
type Buffer struct{ ... }                       // thread-safe rolling buffer
func NewBuffer(cap int) *Buffer
func (*Buffer) Append(p []byte)
func (*Buffer) Snapshot() []byte
func (*Buffer) QuietFor() time.Duration         // for PTY-heartbeat watchdog

func IsIdle(snap []byte) bool                   // ❯ + no spinner
func IsThinking(snap []byte) bool               // ✻ visible
var IdleGlyph, SpinnerGlyph []byte              // ❯, ✻
```

### Rendering

```go
func Render(snap []byte, cols, rows int) string // vt10x-backed grid render
func StripANSI(snap []byte) []byte              // CSI strip (cheap predicate)
func StripANSIString(s string) string
func StripOSC(snap []byte) []byte               // OSC strip
```

### Modal detection + parsers

```go
type ModalClass string                          // typed const
const (
    ModalClassUnknown / MCP / Agents /
    SlashPicker / AskUserQuestion /
    TrustFolder / Permission ModalClass = ...
)
func DetectModalClass(snap []byte) ModalClass

func ParsePicker(snap []byte) []PickerItem       // slash-command picker
func SelectCommand(name string) []byte           // keystrokes to commit
func ParseMcpStatus(snap []byte) *McpStatus      // /mcp modal contents
func ParseAgentList(snap []byte) *AgentList      // /agents modal contents

func HasTrustModal(snap []byte) bool             // first-use trust dialog
func HasMcpFailureBanner(snap []byte) bool       // "N MCP server failed"
func FailedMcpCount(snap []byte) int             // 0 if no banner
```

### State machine + watchdog

```go
type Tracker struct{ ... }
type TrackerOpts struct {
    PTYQuietLimit      time.Duration  // default 30s
    SpinnerFreezeLimit time.Duration  // default 30s
}
func NewTracker(opts TrackerOpts) *Tracker
func (*Tracker) RecordTransition(state string)
func (*Tracker) ObserveSpinner(visible bool, totalSeconds int)
func (*Tracker) CheckWatchdog(buf *Buffer) error
```

### Misc helpers

```go
func EncodeCwd(cwd string) string                  // cwd → projects-dir name
func WaitUntil(ctx, predicate) error               // 50ms-cadence poll
```

## Example

```go
cmd := exec.Command("claude", "--session-id", sessionID)
tuidriver.EnsureClaudeEnv(cmd)

session, err := tuidriver.Spawn(cmd, tuidriver.SpawnOpts{
    Mirror: os.Stderr, // optional — copy of PTY bytes for debugging
})
if err != nil { return err }
defer session.Close()

tr := tuidriver.NewTracker(tuidriver.TrackerOpts{})

// Wait for ❯ idle.
if err := tuidriver.WaitUntil(ctx, func() bool {
    return tuidriver.IsIdle(session.Buffer.Snapshot())
}); err != nil { return err }

// Send a prompt.
session.Write([]byte("What is 2+2?\r"))
tr.RecordTransition("prompt-sent")

// ... watchdog tick:
if err := tr.CheckWatchdog(session.Buffer); err != nil {
    return err  // PTY-quiet or spinner-freeze fired
}
```

The 6 spike binaries under `cmd/` are working examples — they import `pkg/tuidriver/` and compose the primitives into specific workflows:

- `spike-one-turn` — single-turn happy path, ❯ → prompt → end_turn → extracted text
- `spike-multi-turn` — N sequential turns including tool use
- `spike-cancel` — mid-response cancellation + recovery
- `spike-permission` — permission-modal detection + auto-respond
- `spike-multiselect` — slash-command picker / `/mcp` / `/agents` parsers
- `spike-ask-user` — claude-initiated AskUserQuestion modal

## Why this exists

Anthropic's 2026-06-15 Agent SDK billing split moves `claude -p`, the Agent SDK, and "third-party apps that authenticate with your Claude subscription through the Agent SDK" to a metered credit pool. *Interactive* Claude Code in the terminal stays on subscription. Driving `claude` via a real PTY puts the spawned process on the explicitly-subscription-eligible surface — TTY-attached stdin/stdout, no `-p` flag, normal interactive UI. This library is the substrate that makes that approach viable for programmatic consumers (`pyry acp`, etc.).

See [Drop-In Contract](../../obsidian-vault/Second%20Brain/📋%20Projects/2026-04-10%20-%20Pyrycode/Drop-In%20Contract.md) for the strategic context.

## Consumers

- `pyrycode/pyrycode` — `pyry acp` mode (planned)

## License

Private repository. No license declared.
