//go:build linux

package executionruntime

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// crashJobDelay keeps the job waiting through everything that follows the crash: the cleanup's
// grace for leftovers to exit by themselves, then the Node start that kills them.
const crashJobDelay = 20 * time.Second

// TestRealNodeCrashLetsTheRestartedNodeKillTheBackgroundJob is the v2 evidence for circus#1373:
// when Node itself is SIGKILLed while a tool job of its Agent runs in the background, the job
// does not keep the project's runtime from starting again. Cleanup has the project's own Node
// kill it, the job is dead before the stop of the lost session is confirmed and before the next
// session's Agent runs, and nothing writes the checkout afterwards.
func TestRealNodeCrashLetsTheRestartedNodeKillTheBackgroundJob(t *testing.T) {
	cases := []struct {
		name string
		// next uses the project after the crash and returns once the job must be dead.
		next func(t *testing.T, f *runtimeFixture, project store.Project, crashed execution.Ref, answered <-chan bool)
	}{{
		// The next conversation on the project starts a new Node only after cleanup proved the
		// old one's processes gone.
		name: "next session",
		next: func(t *testing.T, f *runtimeFixture, project store.Project, _ execution.Ref, answered <-chan bool) {
			ref, err := f.service.Start(f.ctx, f.scope, execution.StartRequest{Caller: execution.Caller{Kind: execution.CallerChat, ID: "after-crash", AttemptID: "first"}, Target: execution.Target{ProjectID: project.ID}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("Reply with the fixture answer.")})
			if err != nil {
				t.Fatalf("new conversation after the Node crash: %v", err)
			}
			result, _ := observeTurn(t, f, ref, 0)
			requireRealFixtureResult(t, result)
			select {
			case alive := <-answered:
				if alive {
					t.Fatal("the next session's Agent ran while the crashed session's job was alive")
				}
			default:
				t.Fatal("the next session's Agent never asked the model")
			}
		},
	}, {
		// Ending the lost session is what delivery of its turn waits for: confirmed only once
		// the job is gone, so the snapshot cannot race it.
		name: "End",
		next: func(t *testing.T, f *runtimeFixture, _ store.Project, crashed execution.Ref, _ <-chan bool) {
			if err := f.service.End(f.ctx, f.scope, crashed.SessionID); err != nil {
				t.Fatalf("End of the session whose Node crashed: %v", err)
			}
		},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			hold := make(chan struct{})
			// job is the background job's PID once known; the next session's first model request
			// reports whether it was still alive then.
			var job atomic.Int64
			answered := make(chan bool, 1)
			fixture := backgroundJobFixture{delay: crashJobDelay, hold: hold, answered: func() {
				if pid := int(job.Load()); pid != 0 {
					select {
					case answered <- jobAlive(pid):
					default:
					}
				}
			}}
			f, driverRoot, project := backgroundJobRuntime(t, ctx, fixture)
			// Release the held answer before the runtime's own cleanup waits for its turn.
			t.Cleanup(func() { close(hold) })

			crashed, err := f.service.Start(ctx, f.scope, execution.StartRequest{Caller: execution.Caller{Kind: execution.CallerChat, ID: "crash-conversation", AttemptID: "first"}, Target: execution.Target{ProjectID: project.ID}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("Start the requested background job with the shell tool.")})
			if err != nil {
				t.Fatal(err)
			}
			pidFile := awaitCheckoutFile(t, ctx, driverRoot, "ora-v2-job.pid")
			pid := readJobPID(t, pidFile)
			job.Store(int64(pid))
			t.Cleanup(func() {
				if jobAlive(pid) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			if !jobAlive(pid) {
				t.Fatalf("background job %d was not running when Node was killed", pid)
			}
			killNode(t, driverRoot)
			if result, _ := observeTurn(t, f, crashed, 0); result.Status == execution.StatusCompleted {
				t.Fatalf("turn whose Node was killed completed: %+v", result)
			}
			crashedAt := time.Now()
			tc.next(t, f, project, crashed, answered)
			if jobAlive(pid) {
				t.Fatalf("background job %d outlived the Node crash after %s", pid, tc.name)
			}
			t.Logf("%s returned %v after the turn ended with the Node crash; job %d is dead", tc.name, time.Since(crashedAt).Round(time.Millisecond), pid)
			// Watch past the job's own deadline, so a job that escaped the kill shows up as its write.
			late := filepath.Join(filepath.Dir(pidFile), "ora-v2-late.txt")
			deadline := time.Now().Add(crashJobDelay + 2*time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(late); err == nil {
					t.Fatalf("background job wrote %s after %s", late, tc.name)
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(100 * time.Millisecond):
				}
			}
		})
	}
}

// awaitCheckoutFile waits for the Agent's tool to create the one file named name in a checkout.
func awaitCheckoutFile(t *testing.T, ctx context.Context, root, name string) string {
	t.Helper()
	for {
		if found := checkoutFiles(t, root, name); len(found) == 1 {
			return found[0]
		} else if len(found) > 1 {
			t.Fatalf("found %d files named %s, want 1: %v", len(found), name, found)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no %s appeared: %v", name, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// killNode SIGKILLs the one Node running from a project runtime root under driverRoot, found by
// the executable path the runtime copied it to, and waits until it has exited.
func killNode(t *testing.T, driverRoot string) {
	t.Helper()
	binaries, err := filepath.Glob(filepath.Join(driverRoot, "*", "w", "nb"))
	if err != nil || len(binaries) != 1 {
		t.Fatalf("Node binaries under %s: %v %v", driverRoot, binaries, err)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if exe, err := os.Readlink(filepath.Join("/proc", entry.Name(), "exe")); err != nil || exe != binaries[0] {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(10 * time.Second); ; {
			// The runtime reaps its child, so the entry disappears once Node has exited.
			if _, err := os.Stat(filepath.Join("/proc", entry.Name())); os.IsNotExist(err) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("Node %d did not exit after SIGKILL", pid)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Fatalf("no running Node from %s", binaries[0])
}
