package boot_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/boot"
)

var errBroken = errors.New("broken")

// steps records every startup step it runs, by name, and fails the one named failing.
type steps struct {
	calls   []string
	failing string
}

func (s *steps) run(name string) error {
	s.calls = append(s.calls, name)
	if name == s.failing {
		return errBroken
	}
	return nil
}

type recoverer struct {
	s    *steps
	name string
}

func (r recoverer) Recover(context.Context) error { return r.s.run(r.name) }

type interrupter struct {
	s    *steps
	name string
}

func (i interrupter) Interrupt(context.Context) error { return i.s.run(i.name) }

type workflows struct{ s *steps }

func (w workflows) SweepInterrupted(context.Context) ([]string, error) {
	return []string{"interrupted"}, w.s.run("workflow-runs")
}
func (w workflows) ResumeInterrupted(_ context.Context, ids []string) error {
	if !slices.Equal(ids, []string{"interrupted"}) {
		return errors.New("missing interrupted workflow IDs")
	}
	return w.s.run("workflow-resume")
}

type deliveries struct{ s *steps }

func (d deliveries) RecoverTurnDeliveries(context.Context) error { return d.s.run("deliveries") }

type decompositions struct{ s *steps }

func (d decompositions) Recover(context.Context) error { return d.s.run("decompositions") }
func (d decompositions) Adopt(context.Context) error   { return d.s.run("decompositions-adopt") }

func TestRecoverDeliversBeforeWorkflowResumeAndAdmission(t *testing.T) {
	all := []string{"task-workflows", "decompositions", "workflow-runs", "task-runs", "deliveries", "workflow-resume", "decompositions-adopt"}
	for i, failing := range append([]string{""}, all...) {
		name := failing
		if name == "" {
			name = "all succeed"
		}
		t.Run(name, func(t *testing.T) {
			s := &steps{failing: failing}
			err := boot.Recover(context.Background(), boot.Hosts{
				TaskWorkflows:  recoverer{s, "task-workflows"},
				Decompositions: decompositions{s},
				WorkflowRuns:   workflows{s},
				Deliveries:     deliveries{s},
				TaskRuns:       interrupter{s, "task-runs"},
			})
			if (failing != "") != errors.Is(err, errBroken) {
				t.Fatalf("error = %v, want the broken step's error only when a step fails", err)
			}
			// Task owners settle before the sweep, adoption follows it, and the first error stops
			// every later step.
			want := all
			if failing != "" {
				want = all[:i]
			}
			if !slices.Equal(s.calls, want) {
				t.Fatalf("steps = %v, want %v", s.calls, want)
			}
		})
	}
}
