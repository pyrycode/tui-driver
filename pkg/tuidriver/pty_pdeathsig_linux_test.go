//go:build linux

package tuidriver

import (
	"os/exec"
	"syscall"
	"testing"
)

// TestStartPTYSetsParentDeathSignal asserts StartPTY wires the Linux
// parent-death SIGKILL onto the spawned command and that creack/pty's own spawn
// attributes co-exist with it.
//
// Linux/CI-only: this file is build-excluded on the darwin dev box, so it does
// NOT gate a local `make check`. It is the real behavioural spec for the
// Pdeathsig wiring; the darwin-runnable proof is the cross-GOOS `go build`
// guard (AC #3).
//
// A full parent-death test (kill the parent, assert the child dies) is
// deliberately omitted: when the parent dies the PTY master FD closes and
// SIGHUPs the whole foreground group, killing the child regardless of
// Pdeathsig — a false green. Isolating the mechanism would need the child to
// ignore SIGHUP, the same confound #168's group-kill test hit. See spec #169.
func TestStartPTYSetsParentDeathSignal(t *testing.T) {
	cmd := exec.Command("cat")
	ptmx, err := StartPTY(cmd)
	if err != nil {
		t.Fatalf("StartPTY: %v", err)
	}
	t.Cleanup(func() {
		_ = ptmx.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil; setParentDeathSignal did not allocate it")
	}
	if got := cmd.SysProcAttr.Pdeathsig; got != syscall.SIGKILL {
		t.Errorf("Pdeathsig = %v, want %v", got, syscall.SIGKILL)
	}
	// creack/pty adds Setsid/Setctty on top of our pre-set SysProcAttr; proving
	// both survive shows the "preserves a pre-set struct" invariant holds in
	// both directions — our nil-allocation wasn't clobbered, and creack's
	// additions didn't drop Pdeathsig.
	if !cmd.SysProcAttr.Setsid {
		t.Error("Setsid = false, want true (creack/pty should have set it)")
	}
	if !cmd.SysProcAttr.Setctty {
		t.Error("Setctty = false, want true (creack/pty should have set it)")
	}
}
