package workflowhost

import (
	"context"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// lifecycleEntries reports how many runs have a lifecycle lock entry.
func lifecycleEntries(h *Host) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.lifecycles)
}

// L2 (review 3): a resume refused as settling because a cancellation holds the lock leaves
// the lock's user count as it found it, so the entry disappears with the cancellation (Z6).
func TestRefusedResumeDuringACancellationLeavesNoLifecycleEntry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, siblings, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	pair := f.exec.awaitStarts(t, 2)
	f.exec.finish(pair["d"], completed("D"))
	f.exec.finish(pair["a"], sessionFailed)
	f.await(t, id, terminal)
	awaitIdle(t, f, id)
	unlock := f.host.lockLifecycle(id) // a cancellation under way
	if _, err := f.host.ResumeRun(ctx, f.scope, id); refusal(t, err) != workflow.ResumeRefusedSettling {
		unlock()
		t.Fatalf("resume during a cancellation = %v", err)
	}
	unlock()
	if n := lifecycleEntries(f.host); n != 0 {
		t.Fatalf("a refused resume left %d lifecycle entries", n)
	}
}

// L2 (review 3): with one holder and two waiters, the entry outlives the first release, so a
// caller arriving while the next waiter holds the lock finds that lock taken instead of a fresh
// one (Z7), and the entry goes once the last waiter is done.
func TestLifecycleLockExcludesACallerArrivingBetweenTwoWaiters(t *testing.T) {
	h := &Host{runs: map[string]*runState{}, lifecycles: map[string]*lifecycleLock{}}
	users := func() int {
		h.mu.Lock()
		defer h.mu.Unlock()
		if l := h.lifecycles["r"]; l != nil {
			return l.users
		}
		return 0
	}
	unlockFirst := h.lockLifecycle("r")
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	for range 2 {
		go func() {
			defer func() { done <- struct{}{} }()
			unlock := h.lockLifecycle("r")
			held <- struct{}{}
			<-release
			unlock()
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for users() != 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the waiters never joined: %d users", users())
		}
		time.Sleep(time.Millisecond)
	}
	unlockFirst()
	<-held // the second caller holds the lock; the third still waits
	if unlock, ok := h.tryLockLifecycle("r"); ok {
		unlock()
		t.Fatal("a caller arriving between two waiters took the lock while a waiter held it")
	}
	release <- struct{}{}
	<-held
	release <- struct{}{}
	<-done
	<-done
	if n := lifecycleEntries(h); n != 0 {
		t.Fatalf("the lock left %d entries once everyone was done", n)
	}
}
