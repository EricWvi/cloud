package store

import "context"

// Who may act on a task. Ending a task, and sending it back from 待验收, is the decision of whoever
// asked for the work or the workspace owner (canCloseTask). Editing, reassigning and splitting it
// also belong to whoever currently does the work: the person it is assigned to, or any current
// member of its squad. An Agent task has no person doing it, so only its creator and the owner
// manage it; running its Agent stays open to every member (AuthorizeTaskRunner).

// TaskFinished reports whether the task is done or cancelled; a finished task is read-only.
func TaskFinished(task Task) bool {
	return task.Status == TaskDone || task.Status == TaskCancelled
}

// CanManageTask reports whether the caller may edit, reassign or split the task. inSquad answers
// squad membership, from the caller's transaction when there is one.
func CanManageTask(ctx context.Context, a Access, task Task, inSquad func(context.Context, string, string, string) (bool, error)) (bool, error) {
	if canCloseTask(a, task) {
		return true, nil
	}
	if task.Assignee == nil {
		return false, nil
	}
	switch task.Assignee.Type {
	case AssigneeHuman:
		return task.Assignee.ID == a.UserID, nil
	case AssigneeSquad:
		return inSquad(ctx, a.TenantID, task.Assignee.ID, a.UserID)
	}
	return false, nil
}

// CanStopTaskRun reports whether the caller may stop the task's run: whoever started it, and
// whoever may end the task.
func CanStopTaskRun(a Access, task Task, run TaskRun) bool {
	return run.StartedBy == a.UserID || canCloseTask(a, task)
}

// canManageTaskTx is CanManageTask inside a transaction.
func canManageTaskTx(ctx context.Context, q dbTX, a Access, task Task) (bool, error) {
	return CanManageTask(ctx, a, task, func(ctx context.Context, tenant, squad, user string) (bool, error) {
		return inSquad(ctx, q, tenant, squad, user)
	})
}
