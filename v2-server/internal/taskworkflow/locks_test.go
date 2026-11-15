package taskworkflow

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"
)

func TestTaskLocksShareWaitersAndReclaim(t *testing.T) {
	h := &Host{locks: map[string]*taskLock{}}
	release := h.lock("same")
	acquired := make(chan struct{})
	finished := make(chan struct{})
	go func() { defer close(finished); unlock := h.lock("same"); close(acquired); unlock() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Observe a queued owner, not a sleep that might merely mean the goroutine was not scheduled.
	for {
		h.mu.Lock()
		users := h.locks["same"].users
		h.mu.Unlock()
		if users == 2 {
			break
		}
		select {
		case <-acquired:
			release()
			<-finished
			t.Fatal("same task did not share a lock")
		case <-ctx.Done():
			release()
			<-finished
			t.Fatal("waiter never joined the task lock")
		default:
			runtime.Gosched()
		}
	}
	select {
	case <-acquired:
		t.Fatal("same-task retry overtook the holder")
	default:
	}
	// A different task must not wait behind this one.
	h.lock("different")()
	release()
	<-finished
	for i := 0; i < 100; i++ {
		h.lock(fmt.Sprint(i))()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.locks) != 0 {
		t.Fatalf("idle locks retained: %d", len(h.locks))
	}
}

func TestTaskLockThirdCallerWaitsForQueuedWaiter(t *testing.T) {
	h := &Host{locks: map[string]*taskLock{}}
	releaseFirst := h.lock("same")
	waiterAcquired := make(chan struct{})
	releaseWaiter := make(chan struct{})
	waiterDone := make(chan struct{})
	defer func() {
		select {
		case <-releaseWaiter:
		default:
			close(releaseWaiter)
		}
	}()
	go func() {
		defer close(waiterDone)
		unlock := h.lock("same")
		close(waiterAcquired)
		<-releaseWaiter
		unlock()
	}()
	users := func() int {
		h.mu.Lock()
		defer h.mu.Unlock()
		if l := h.locks["same"]; l != nil {
			return l.users
		}
		return 0
	}
	deadline := time.After(5 * time.Second)
	for users() != 2 {
		select {
		case <-deadline:
			releaseFirst()
			t.Fatal("waiter never queued")
		default:
			runtime.Gosched()
		}
	}
	releaseFirst()
	select {
	case <-waiterAcquired:
	case <-time.After(5 * time.Second):
		t.Fatal("queued waiter never acquired the lock")
	}

	thirdAcquired := make(chan struct{})
	go func() {
		unlock := h.lock("same")
		unlock()
		close(thirdAcquired)
	}()
	// One deadline created before the loop: a time.After inside the select would allocate a fresh
	// timer per spin that never fires, so a stuck third caller would spin to the package timeout.
	deadline = time.After(5 * time.Second)
	for users() != 2 {
		select {
		case <-thirdAcquired:
			t.Fatal("third caller bypassed the queued waiter")
		case <-deadline:
			t.Fatal("third caller never joined the existing lock")
		default:
			runtime.Gosched()
		}
	}
	select {
	case <-thirdAcquired:
		t.Fatal("third caller acquired before the queued waiter released")
	default:
	}
	close(releaseWaiter)
	select {
	case <-waiterDone:
	case <-time.After(5 * time.Second):
		t.Fatal("queued waiter did not finish")
	}
	select {
	case <-thirdAcquired:
	case <-time.After(5 * time.Second):
		t.Fatal("third caller did not follow the queued waiter")
	}
}
