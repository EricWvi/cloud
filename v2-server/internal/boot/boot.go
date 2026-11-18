// Package boot orders the startup recovery of what a previous process left live, so main and the
// tests that pin the order run the same sequence.
//
// The workflow host's sweep may absorb an interrupted Iteration or pending LoopExit cleanup and
// resume it (desktop's composite recovery), so some workflow runs outlive the process that started
// them. Runs owned by a task — a task's handling run and a task decomposition's run — must not:
// their owners settle them first, cancelling every one still live, so the sweep never absorbs
// one and its owner's record cannot disagree with a run that carries on. It also holds after a
// settlement: a run its owner settled is never reopened in place, as resume and the review
// form's task creation retry refuse task-owned runs (owner decisions D25, D30 and D32), so no
// settled task run turns live again for a later sweep to absorb. The one exception is a
// decomposition run at a submitted review: the sweep never absorbs it (form runs are excluded and
// only their deterministic review work is replayed), so it is left to the review recovery and its
// decomposition is adopted after the sweep.
package boot

import "context"

// Recoverer settles what a previous process left live for one owner of workflow runs.
type Recoverer interface {
	Recover(ctx context.Context) error
}

// Interrupter ends what a previous process left executing.
type Interrupter interface {
	Interrupt(ctx context.Context) error
}

// Adopter takes over, after the workflow sweep, what its Recover step left to the sweep.
type Adopter interface {
	Recoverer
	Adopt(ctx context.Context) error
}

// WorkflowRecovery separates durable interruption from any subsequent workflow dispatch.
type WorkflowRecovery interface {
	SweepInterrupted(context.Context) ([]string, error)
	ResumeInterrupted(context.Context, []string) error
}

// DeliveryRecovery saves the checkouts of concluded turns before new work can run.
type DeliveryRecovery interface {
	RecoverTurnDeliveries(context.Context) error
}

// Hosts are the startup steps, one per owner of state a previous process can leave live.
type Hosts struct {
	// TaskWorkflows cancels the live handling runs of tasks and settles their links.
	TaskWorkflows Recoverer
	// Decompositions cancels the live runs of task decompositions and fails the decompositions,
	// except runs at a submitted review, which it adopts once the sweep has replayed the review.
	Decompositions Adopter
	// WorkflowRuns fails, or absorbs and resumes, the workflow runs left executing.
	WorkflowRuns WorkflowRecovery
	Deliveries   DeliveryRecovery
	// TaskRuns ends the task Agent turns left live.
	TaskRuns Interrupter
}

// Recover runs the startup steps in their required order and stops at the first error. Task-owned
// runs are settled before the workflow sweep, which must therefore come after both of them; the
// decompositions left to the sweep's review recovery are adopted right after it. It must
// complete before the process serves requests.
func Recover(ctx context.Context, h Hosts) error {
	if err := h.TaskWorkflows.Recover(ctx); err != nil {
		return err
	}
	if err := h.Decompositions.Recover(ctx); err != nil {
		return err
	}
	ids, err := h.WorkflowRuns.SweepInterrupted(ctx)
	if err != nil {
		return err
	}
	if err := h.TaskRuns.Interrupt(ctx); err != nil {
		return err
	}
	if h.Deliveries != nil {
		if err := h.Deliveries.RecoverTurnDeliveries(ctx); err != nil {
			return err
		}
	}
	if err := h.WorkflowRuns.ResumeInterrupted(ctx, ids); err != nil {
		return err
	}
	return h.Decompositions.Adopt(ctx)
}
