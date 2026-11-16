package workflow

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
)

// Resume refusals, the preview's reason and the resume error's reason. Desktop answers every one
// of them with WorkflowRunNotResumable; v2 names which rule refused so the page can explain it.
const (
	// ResumeRefusedStatus: only a failed or cancelled run resumes
	// (desktop@ebd5b725 crates/application/src/workflow_run/engine/engine.rs:183-188).
	ResumeRefusedStatus = "run_not_ended"
	// ResumeRefusedRunning: a node of the run still executes, the idle gate
	// (desktop@ebd5b725 crates/db/src/repository/workflow_run_engine/resume.rs:36-56).
	ResumeRefusedRunning = "node_running"
	// ResumeRefusedNothing: no failed or cancelled node to resume from
	// (desktop@ebd5b725 crates/application/src/workflow_run/engine/engine.rs:192-194).
	ResumeRefusedNothing = "no_failed_node"
	// ResumeRefusedStuck: v2 only. The nodes that block the idle gate are swift rows (Start,
	// Output, Condition, Aggregator) a same-wave failure left running. Desktop leaves them the same
	// way (engine.rs:406-439), but nothing ever completes them under an ended run, so waiting
	// cannot help; the page sends the user to a new run instead.
	ResumeRefusedStuck = "node_stuck"
	// ResumeRefusedForm, ResumeRefusedFormRetry, ResumeRefusedFormSubmitted: v2 only. A review
	// form is frozen once per run, so a resume that would rerun the form node is refused. The
	// form node is cleared when it was cancelled at the review (its form never submitted:
	// ResumeRefusedForm), or as a descendant of a restarting container, where its form may be
	// submitted already. Then the form's own retry (POST …/form/retry) applies when its
	// preconditions hold, a failed run with a failed task creation (ResumeRefusedFormRetry);
	// otherwise a submitted form cannot be filled again and only a new run can go on
	// (ResumeRefusedFormSubmitted). A failure in task creation that clears no container keeps the
	// form node and resumes normally, rerunning the task creation.
	ResumeRefusedForm          = "review_form"
	ResumeRefusedFormRetry     = "review_form_retry"
	ResumeRefusedFormSubmitted = "review_form_submitted"
	// ResumeRefusedTaskHandling: v2 only (owner decision D25, 2026-11-15). A run a task started
	// as its handling run settles its task link once; resuming it would execute again for the
	// task with the link already settled, so the task's handling is started again from the task.
	// Review-source (inspection, patrol) links do not move the task's status and stay resumable.
	ResumeRefusedTaskHandling = "task_handling"
	// ResumeRefusedTaskDecomposition: v2 only (owner decision D32, 2026-11-16). A task
	// decomposition settles once, when its run ends; a resumed run would carry on detached from it
	// (its subtasks never written) and, being no longer tied to a running decomposition, would be
	// absorbed by the boot sweep across a restart. The task decomposes again instead.
	ResumeRefusedTaskDecomposition = "task_decomposition"
	// ResumeRefusedSettling: v2 only. The execution bridge is still starting or ending an Agent
	// turn of this run (a cancellation's turn has not finished ending), so the resume must wait.
	ResumeRefusedSettling = "run_settling"
)

// ResumeState is what resume planning reads about a run: its stored status, every live attempt
// of every scope (root, Iteration rounds and Loop rounds alike) and the nodes holding a frozen
// review form, and of those the ones whose form was submitted, and whether a task owns the run as
// its handling run or as its decomposition's run.
type ResumeState struct {
	Status               string
	Nodes                []NodeRun
	FormNodeIDs          []string
	SubmittedFormNodeIDs []string
	TaskHandling         bool
	TaskDecomposition    bool
}

// ResumeFailedNode is one node a resume starts from: it has a failed or cancelled live attempt.
// Container is the Loop or Iteration that restarts in its place, empty for an ordinary node.
type ResumeFailedNode struct {
	NodeID, NodeRunID, Status string
	Container                 string
}

// ResumePlan is the outcome of planning a resume from failure, shared by the preview and by the
// resume transaction so both apply the same rules to the same rows. Refusal is empty when the
// run can resume; Running lists the nodes that block it under ResumeRefusedRunning.
type ResumePlan struct {
	Refusal    string
	Running    []string
	Failed     []ResumeFailedNode
	Containers []string
	// Clear lists, sorted, every node whose live attempts the resume soft-deletes: the failed
	// nodes, their transitive successors and each restarting container with its members.
	Clear []string
}

// ResumeRefusal is the error a resume returns when its plan refuses it.
type ResumeRefusal struct {
	Reason string
	// Running lists the blocking nodes when Reason is ResumeRefusedRunning.
	Running []string
}

func (e *ResumeRefusal) Error() string {
	return "workflow run cannot resume from failure: " + e.Reason
}

// ResumeRepository is the persistence a resume needs beyond Repository. ResumeState reads without
// locking, for the preview; ResumeRun locks the run, hands the locked state to plan, and when plan
// returns node ids soft-deletes their attempts (and the rounds of every cleared Loop), applies
// clear to the run payload and returns the run to running, all in one transaction. An error
// from plan aborts the transaction unchanged.
type ResumeRepository interface {
	ResumeState(ctx context.Context, runID string) (ResumeState, error)
	ResumeRun(ctx context.Context, runID string, plan func(ResumeState) ([]string, error), clear func(payload []byte, cleared []string) ([]byte, error)) error
}

// errNoResume reports a repository that cannot resume runs.
var errNoResume = errors.New("workflow repository does not support resume")

// PreviewResume plans a resume from failure without changing anything.
func (e *Engine) PreviewResume(ctx context.Context, runID string) (ResumePlan, error) {
	repo, ok := e.repo.(ResumeRepository)
	if !ok {
		return ResumePlan{}, errNoResume
	}
	run, err := e.repo.RunContext(ctx, runID)
	if err != nil {
		return ResumePlan{}, err
	}
	g, err := ParseGraph(run.Graph)
	if err != nil {
		return ResumePlan{}, err
	}
	st, err := repo.ResumeState(ctx, runID)
	if err != nil {
		return ResumePlan{}, err
	}
	return planResume(g, st), nil
}

// ResumeFromFailure is desktop's resume_from_failure (desktop@ebd5b725
// crates/application/src/workflow_run/engine/engine.rs:179-211): under the run gate it replans
// from the locked rows, clears the failed or cancelled nodes with every descendant and every
// restarting container, returns the same run to running and reschedules it through the normal
// scheduling pass. Succeeded nodes keep their attempts and pool values. A refused plan returns a
// *ResumeRefusal.
func (e *Engine) ResumeFromFailure(ctx context.Context, runID string) error {
	defer e.gate(runID)()
	repo, ok := e.repo.(ResumeRepository)
	if !ok {
		return errNoResume
	}
	run, err := e.repo.RunContext(ctx, runID)
	if err != nil {
		return err
	}
	g, err := ParseGraph(run.Graph)
	if err != nil {
		return err
	}
	err = repo.ResumeRun(ctx, runID, func(st ResumeState) ([]string, error) {
		plan := planResume(g, st)
		if plan.Refusal != "" {
			return nil, &ResumeRefusal{Reason: plan.Refusal, Running: plan.Running}
		}
		return plan.Clear, nil
	}, clearResumedPayload)
	if err != nil {
		return err
	}
	// The run left running through a failure or a cancellation, both of which settled its waits
	// in PostgreSQL. What the process still holds for it, a cancellation fence or a timer for a
	// wait that is now soft-deleted, must not outlive the resume: a fence would swallow the next
	// retryable failure of the resumed run.
	e.retries.forget(runID)
	e.publish(runID)
	return e.schedule(ctx, runID)
}

// FenceRetriesForCancel is CancelRetries for a cancellation that must also learn the run's current
// status: both happen under the run gate, so a resume committed before it is seen as running and
// one attempted after it finds the fence. The status is read first: when it cannot be read the
// cancellation goes no further and the run keeps both its retry timers and its unfenced state,
// because nothing would re-arm a timer cancelled here (armRetry runs only on a failure or a wake).
// Reading before fencing changes nothing for a concurrent resume or wake, which wait on the same
// gate. ReleaseRetries drops the fence as for CancelRetries.
func (e *Engine) FenceRetriesForCancel(ctx context.Context, runID string) (string, error) {
	defer e.gate(runID)()
	run, err := e.repo.RunContext(ctx, runID)
	if err != nil {
		return "", err
	}
	t := &e.retries
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cancelled[runID] = true
	for _, w := range t.waits[runID] {
		w.cancel()
	}
	return run.Status, nil
}

// forget cancels a run's armed retry timers and drops its cancellation fence. Callers hold the
// run gate and have just returned the run to running with no waiting attempt left.
func (t *retryTimers) forget(runID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, w := range t.waits[runID] {
		w.cancel()
	}
	delete(t.cancelled, runID)
}

// planResume applies desktop's resume rules to the live rows: the status check, the idle gate
// (running Loop and Iteration rows are parked containers, not work in flight;
// desktop@ebd5b725 crates/application/src/workflow_run/engine/region.rs:12-24) and the clear set
// of resume_clear_node_ids (region.rs:86-120).
func planResume(g *Graph, st ResumeState) ResumePlan {
	plan := ResumePlan{Running: []string{}, Failed: []ResumeFailedNode{}, Containers: []string{}, Clear: []string{}}
	if st.Status != StatusFailed && st.Status != StatusCancelled {
		plan.Refusal = ResumeRefusedStatus
		return plan
	}
	if st.TaskHandling {
		plan.Refusal = ResumeRefusedTaskHandling
		return plan
	}
	if st.TaskDecomposition {
		plan.Refusal = ResumeRefusedTaskDecomposition
		return plan
	}
	seen := map[string]bool{}
	stuck := true
	for _, r := range st.Nodes {
		if r.Status == StatusRunning && r.NodeType != "iteration" && r.NodeType != "loop" && !seen[r.NodeID] {
			seen[r.NodeID] = true
			plan.Running = append(plan.Running, r.NodeID)
			stuck = stuck && runtimes[r.NodeType].swift != nil
		}
	}
	if len(plan.Running) != 0 {
		plan.Refusal = ResumeRefusedRunning
		if stuck {
			plan.Refusal = ResumeRefusedStuck
		}
		return plan
	}
	clear := map[string]bool{}
	containers := map[string]bool{}
	failed := map[string]bool{}
	for _, r := range st.Nodes {
		if (r.Status != StatusFailed && r.Status != StatusCancelled) || failed[r.NodeID] {
			continue
		}
		failed[r.NodeID] = true
		owner := g.resumeUnitOwner(r.NodeID)
		plan.Failed = append(plan.Failed, ResumeFailedNode{NodeID: r.NodeID, NodeRunID: r.ID, Status: r.Status, Container: owner})
		if owner != "" {
			containers[owner] = true
			clear[owner] = true
			for _, member := range g.resumeUnitMembers(owner) {
				clear[member] = true
			}
			for _, successor := range g.transitiveSuccessors(owner) {
				clear[successor] = true
			}
			continue
		}
		clear[r.NodeID] = true
		for _, successor := range g.transitiveSuccessors(r.NodeID) {
			clear[successor] = true
		}
	}
	plan.Containers = sortedKeys(containers)
	plan.Clear = sortedKeys(clear)
	if len(plan.Clear) == 0 {
		plan.Refusal = ResumeRefusedNothing
		return plan
	}
	cleared := func(ids []string) bool { return slices.ContainsFunc(ids, func(id string) bool { return clear[id] }) }
	switch {
	case !cleared(st.FormNodeIDs):
	case !cleared(st.SubmittedFormNodeIDs):
		plan.Refusal = ResumeRefusedForm
	case st.Status == StatusFailed && slices.ContainsFunc(st.Nodes, func(r NodeRun) bool { return r.NodeType == "createTasks" && r.Status == StatusFailed }):
		// RetryWorkflowFormTasks' precondition: a failed run, a submitted form, a failed createTasks.
		plan.Refusal = ResumeRefusedFormRetry
	default:
		plan.Refusal = ResumeRefusedFormSubmitted
	}
	return plan
}

// resumeUnitOwner is desktop's resume_unit_owner_id (region.rs:56-70): an Iteration or Loop is
// its own unit, a region member or Loop body node resumes through its container, and an ordinary
// node has no unit owner.
func (g *Graph) resumeUnitOwner(id string) string {
	if n := g.root.byID[id]; n != nil {
		if n.kind == "iteration" || n.kind == "loop" {
			return id
		}
		return n.owner
	}
	for _, n := range g.root.nodes {
		if n.kind == "loop" && g.scopes[n.id] != nil && g.scopes[n.id].byID[id] != nil {
			return n.id
		}
	}
	return ""
}

// resumeUnitMembers is desktop's resume_unit_member_ids (region.rs:72-82): the region members of
// an Iteration or the body nodes of a Loop.
func (g *Graph) resumeUnitMembers(owner string) []string {
	members := []string{}
	if n := g.root.byID[owner]; n != nil && n.kind == "loop" {
		if body := g.scopes[owner]; body != nil {
			for _, m := range body.nodes {
				members = append(members, m.id)
			}
		}
		return members
	}
	for _, m := range g.root.nodes {
		if !m.container && m.owner == owner {
			members = append(members, m.id)
		}
	}
	return members
}

// transitiveSuccessors lists every root-graph node reachable from id, excluding id.
func (g *Graph) transitiveSuccessors(id string) []string {
	seen := map[string]bool{id: true}
	queue := []string{id}
	result := []string{}
	for len(queue) != 0 {
		current := queue[0]
		queue = queue[1:]
		for _, e := range g.root.edges {
			if e.source == current && !seen[e.target] {
				seen[e.target] = true
				result = append(result, e.target)
				queue = append(queue, e.target)
			}
		}
	}
	sort.Strings(result)
	return result
}

// clearResumedPayload removes what cleared nodes wrote into the run payload, as desktop's resume
// transaction (desktop@ebd5b725 crates/db/src/repository/workflow_run_engine/resume.rs:104-129):
// pool values whose declared writer is cleared (the catalog stays), their Condition decisions,
// Iteration ledgers and per-round Iteration decisions. Loop round state lives in the round scopes,
// which the store closes with their rows.
func clearResumedPayload(raw []byte, cleared []string) ([]byte, error) {
	p, ok, err := DecodeRunPayload(raw)
	if err != nil || !ok {
		return raw, err
	}
	gone := map[string]bool{}
	for _, id := range cleared {
		gone[id] = true
	}
	for selector := range p.VariablePool.Values {
		if def, declared := p.VariablePool.Catalog[selector]; declared && gone[def.Writer] {
			delete(p.VariablePool.Values, selector)
		}
	}
	p.VariablePool.Revision++
	for id := range p.ConditionDecisions {
		if gone[id] {
			delete(p.ConditionDecisions, id)
		}
	}
	for owner := range p.IterationLedger {
		if gone[owner] {
			delete(p.IterationLedger, owner)
		}
	}
	for key := range p.IterationConditionDecisions {
		id, _, _ := strings.Cut(key, "#")
		if gone[id] {
			delete(p.IterationConditionDecisions, key)
		}
	}
	return p.Encode()
}
