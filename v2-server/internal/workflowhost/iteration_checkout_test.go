package workflowhost

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/executionruntime"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// checkoutDriver stands in for the Agent driver behind the real execution service. Like the real
// one, it clones a workspace's checkout on its first session and records it, and starts every
// later session of the workspace on that checkout, or on the one an earlier process recorded
// (Reattach). A turn whose prompt contains hold keeps running until the service shuts down, as a
// turn the restart interrupts.
type checkoutDriver struct {
	db *store.Store
	// name tells this process's clones from an earlier one's.
	name, root, hold string

	mu        sync.Mutex
	clones    int
	checkouts map[string][2]string // workspace -> clone execution, path
	ran       []checkoutRun
	started   chan checkoutRun
}

// checkoutRun is one turn the driver ran: on which workspace and checkout, and whether the edit
// the held turn left in that checkout was there.
type checkoutRun struct {
	prompt, workspace, clone, path string
	sawEdit                        bool
}

func newCheckoutDriver(db *store.Store, name, root, hold string) *checkoutDriver {
	return &checkoutDriver{db: db, name: name, root: root, hold: hold, checkouts: map[string][2]string{}, started: make(chan checkoutRun, 16)}
}

func (d *checkoutDriver) Run(ctx context.Context, _ execution.Scope, session execution.Session, turn execution.Turn, emit func(context.Context, execution.Event) error) (execution.Result, error) {
	prompt, _ := turn.Prompt.Text()
	d.mu.Lock()
	at, ok := d.checkouts[session.WorkspaceID]
	if !ok {
		d.clones++
		id := d.name + "-" + strconv.Itoa(d.clones)
		at = [2]string{"c-" + id, filepath.Join(d.root, "clone-"+id)}
		d.checkouts[session.WorkspaceID] = at
	}
	d.mu.Unlock()
	if !ok {
		if err := os.MkdirAll(at[1], 0o700); err != nil {
			return execution.Result{}, err
		}
		if err := d.db.RecordWorkspace(ctx, session.WorkspaceID, store.WorkspaceLocation{Path: at[1], BaseCommit: "fixed", CloneExecution: at[0]}); err != nil {
			return execution.Result{}, err
		}
	}
	_, err := os.Stat(filepath.Join(at[1], "edit.txt"))
	run := checkoutRun{prompt: prompt, workspace: session.WorkspaceID, clone: at[0], path: at[1], sawEdit: err == nil}
	d.mu.Lock()
	d.ran = append(d.ran, run)
	d.mu.Unlock()
	if strings.Contains(prompt, d.hold) {
		if err := os.WriteFile(filepath.Join(at[1], "edit.txt"), []byte("uncommitted"), 0o600); err != nil {
			return execution.Result{}, err
		}
		d.started <- run
		<-ctx.Done()
		return execution.Result{}, ctx.Err()
	}
	d.started <- run
	result := execution.Result{Status: execution.StatusCompleted, Answer: "done", AnswerComplete: true, StopReason: "end_turn"}
	return result, emit(ctx, execution.Event{Kind: execution.EventTurnEnded, Result: &result})
}

func (*checkoutDriver) End(context.Context, string) error { return nil }
func (*checkoutDriver) Rebuild(string)                    {}
func (*checkoutDriver) Close(context.Context) error       { return nil }

func (d *checkoutDriver) Reattach(workspace, clone, path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.checkouts[workspace] = [2]string{clone, path}
}

func (d *checkoutDriver) next(t *testing.T) checkoutRun {
	t.Helper()
	select {
	case run := <-d.started:
		return run
	case <-time.After(10 * time.Second):
		t.Fatal("no Agent turn ran")
		return checkoutRun{}
	}
}

// A run that survives a restart only through its Iteration (the sweep absorbs the interrupted
// round, no node is parked) keeps its checkout across the whole chain: the execution service of
// the next process restores the run's workspace, the sweep and the engine's resume dispatch the
// next round, and that round's session shares the workspace and starts on the recorded checkout,
// where the interrupted round's edit still is, not on a fresh clone.
func TestAnAbsorbedIterationsNextRoundStartsOnTheRunsCheckout(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	models, err := executionruntime.ModelsFromEnvironment(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	models = executionruntime.WithFixtureModels(models)
	root := t.TempDir()
	start := func(driver *checkoutDriver) (*executionruntime.Service, *Host) {
		t.Helper()
		service, err := executionruntime.New(ctx, executionruntime.Config{Store: f.s, Models: models, Agent: driver})
		if err != nil {
			t.Fatal(err)
		}
		host, err := NewHost(Config{Store: f.s, Execution: service, Catalog: service, StopTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return service, host
	}
	f.host.Close()
	first := newCheckoutDriver(f.s, "first", root, "item 0")
	service, host := start(first)
	f.host = host
	if err := host.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	runID := f.run(t, iterationGraph("continue", []string{"one", "two"}, 50), nil)
	if _, err := host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	interrupted := first.next(t)
	f.await(t, runID, func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
		return iterationRow(rows, "worker", 0).SessionID != nil
	})

	// The process stops while round 0 executes; the next one starts over the same database.
	host.Close()
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	second := newCheckoutDriver(f.s, "second", root, "never")
	service, host = start(second)
	f.host = host
	t.Cleanup(func() {
		host.Close()
		_ = service.Close(context.Background())
	})
	if err := host.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	resumed := second.next(t)
	if !strings.Contains(resumed.prompt, "item 1") {
		t.Fatalf("after the restart ran %q, want the Iteration's next round", resumed.prompt)
	}
	if resumed.workspace != interrupted.workspace || resumed.clone != interrupted.clone || resumed.path != interrupted.path || !resumed.sawEdit {
		t.Fatalf("next round ran on %+v, want the interrupted round's workspace and checkout %+v with its edit", resumed, interrupted)
	}
	run, rows := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusSucceeded || run.Output == nil || *run.Output != `{"failed":1,"items":["done"]}` {
		t.Fatalf("run = %+v", run)
	}
	if errorKind(t, iterationRow(rows, "worker", 0)) != "interrupted_by_restart" {
		t.Fatalf("round 0 = %+v, want it interrupted by the restart", iterationRow(rows, "worker", 0))
	}
}
