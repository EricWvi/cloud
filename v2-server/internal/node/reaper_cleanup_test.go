//go:build linux

package node

import (
	"context"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// reaperExitDelay models the time ora-reaper spends in kill_all after its parent's pipe closes.
const reaperExitDelay = 300 * time.Millisecond

// TestReaperHelper stands in for ora-reaper: it reports readiness on stdout, then exits on its own
// reaperExitDelay after its parent closes stdin. V2_REAPER_HELPER=ignore-term also ignores SIGTERM.
func TestReaperHelper(t *testing.T) {
	mode := os.Getenv("V2_REAPER_HELPER")
	if mode == "" {
		return
	}
	if mode == "ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	if _, err := os.Stdout.WriteString("ready"); err != nil {
		os.Exit(2)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	time.Sleep(reaperExitDelay)
}

// reaperStandIn is a running TestReaperHelper copied into a runtime root as rb.
type reaperStandIn struct {
	parent io.WriteCloser // closing it is the parent Node's death as the reaper sees it
	done   chan struct{}
	state  *os.ProcessState
}

func startReaperStandIn(t *testing.T, paths Paths, mode string) *reaperStandIn {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := copyAsset(testAsset(t, self), filepath.Join(paths.Root, "rb")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(paths.Root, "rb"), "-test.run=^TestReaperHelper$")
	cmd.Env = append(os.Environ(), "V2_REAPER_HELPER="+mode)
	cmd.Dir = paths.Root
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := &reaperStandIn{parent: stdin, done: make(chan struct{})}
	ready := make([]byte, len("ready"))
	_, readErr := io.ReadFull(stdout, ready)
	go func() {
		_, _ = io.Copy(io.Discard, stdout)
		_ = cmd.Wait()
		r.state = cmd.ProcessState
		close(r.done)
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-r.done })
	if readErr != nil || string(ready) != "ready" {
		t.Fatalf("reaper stand-in not ready: %q %v", ready, readErr)
	}
	return r
}

func (r *reaperStandIn) wait(t *testing.T) *os.ProcessState {
	t.Helper()
	select {
	case <-r.done:
		return r.state
	case <-time.After(10 * time.Second):
		t.Fatal("reaper stand-in did not exit")
		return nil
	}
}

// A reaper outliving a killed Node is still SIGKILLing the Git process groups it registered;
// cleanup must let it finish rather than terminate it and leave those groups behind.
func TestCleanupWaitsForReaperToExitBeforeSignalling(t *testing.T) {
	paths, lock, err := claimRoot(socketDirectory(t))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	reaper := startReaperStandIn(t, paths, "exit-after-eof")
	_ = reaper.parent.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	if err := cleanOwned(ctx, paths, 3*time.Second); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if state := reaper.wait(t); !state.Success() {
		t.Fatalf("reaper was signalled instead of finishing its cleanup: %v", state)
	}
	if elapsed := time.Since(started); elapsed < reaperExitDelay-50*time.Millisecond {
		t.Fatalf("cleanup returned after %v, before the reaper finished", elapsed)
	}
}

// A reaper that does not exit within the grace (its parent still holds the pipe, or it hangs)
// is terminated as before: SIGTERM, then SIGKILL after another grace.
func TestCleanupSignalsReaperStillAliveAfterGrace(t *testing.T) {
	const grace = 200 * time.Millisecond
	for _, tc := range []struct {
		mode       string
		signal     syscall.Signal
		minElapsed time.Duration
	}{
		{mode: "exit-after-eof", signal: syscall.SIGTERM, minElapsed: grace},
		{mode: "ignore-term", signal: syscall.SIGKILL, minElapsed: 2 * grace},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			paths, lock, err := claimRoot(socketDirectory(t))
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			reaper := startReaperStandIn(t, paths, tc.mode)
			defer reaper.parent.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			started := time.Now()
			if err := cleanOwned(ctx, paths, grace); err != nil {
				t.Fatalf("cleanup: %v", err)
			}
			elapsed := time.Since(started)
			status, ok := reaper.wait(t).Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != tc.signal {
				t.Fatalf("reaper status = %v, want killed by %v", status, tc.signal)
			}
			if elapsed < tc.minElapsed {
				t.Fatalf("cleanup returned after %v, want at least %v of grace before each signal", elapsed, tc.minElapsed)
			}
		})
	}
}

// Cleanup's waits on a reaper end with the caller's context.
func TestCleanupReaperWaitRespectsContext(t *testing.T) {
	paths, lock, err := claimRoot(socketDirectory(t))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	reaper := startReaperStandIn(t, paths, "exit-after-eof")
	defer reaper.parent.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := cleanOwned(ctx, paths, time.Minute); err == nil {
		t.Fatal("cleanup claimed success with the reaper still running")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("cleanup ignored its context for %v", elapsed)
	}
}
