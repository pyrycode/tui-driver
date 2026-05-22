package tuidriver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SessionJSONLPath returns the path that claude writes its per-session
// JSONL log to for a session pinned via `claude --session-id <sessionID>`
// running with working directory cwd. The home argument is typically
// os.UserHomeDir(); pass it explicitly so the function stays pure and
// testable.
//
// The path is computed as filepath.Join(home, ".claude", "projects",
// EncodeCwd(cwd), sessionID+".jsonl"). EncodeCwd is used internally —
// no separate encoder can drift at the call site.
//
// The function does not validate sessionID (consumers do, e.g. via
// uuid.Parse), does not stat the file, and does not error.
func SessionJSONLPath(home, cwd, sessionID string) string {
	return filepath.Join(home, ".claude", "projects", EncodeCwd(cwd), sessionID+".jsonl")
}

// WaitForSessionJSONL polls path with os.Stat at DefaultPollInterval until
// the file exists or ctx is cancelled. Returns nil when the file appears;
// returns a wrapped context.Cause(ctx) on cancellation / deadline (so
// errors.Is(err, context.DeadlineExceeded) and errors.Is(err,
// context.Canceled) both work). Stat errors that are NOT os.IsNotExist
// are returned wrapped without further polling — permission errors and
// ENOTDIR do not resolve by retry.
//
// Distinct from WaitUntil because stat errors short-circuit the loop;
// WaitUntil's bool-only predicate cannot signal "stop polling".
//
// Use after WritePrompt — interactive claude under --session-id defers
// JSONL creation until first input lands (see
// docs/knowledge/architecture/jsonl-layout.md § "Empirical surprise").
// The caller owns the timeout via ctx — typical usage:
//
//	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
//	defer cancel()
//	err := tuidriver.WaitForSessionJSONL(ctx, path)
func WaitForSessionJSONL(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat session jsonl %s: %w", path, err)
	}
	ticker := time.NewTicker(DefaultPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("session jsonl %s did not appear: %w", path, context.Cause(ctx))
		case <-ticker.C:
			if _, err := os.Stat(path); err == nil {
				return nil
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("stat session jsonl %s: %w", path, err)
			}
		}
	}
}
