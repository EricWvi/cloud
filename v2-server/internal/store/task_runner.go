package store

import (
	"context"
	"errors"
)

// ErrTaskNotRunnable reports a task without an executable assignee.
var ErrTaskNotRunnable = errors.New("task not assigned to an agent, a person or a squad")

// AuthorizeTaskRunner is the shared Agent and workflow start policy. Human tasks belong to that
// person, squad tasks to their current human members, and Agent tasks are runnable by any member.
func AuthorizeTaskRunner(ctx context.Context, task Task, user string, inSquad func(context.Context, string, string, string) (bool, error)) error {
	if task.Assignee == nil {
		return ErrTaskNotRunnable
	}
	switch task.Assignee.Type {
	case AssigneeHuman:
		if task.Assignee.ID != user {
			return ErrTaskNotAssignee
		}
	case AssigneeSquad:
		member, err := inSquad(ctx, task.TenantID, task.Assignee.ID, user)
		if err != nil {
			return err
		}
		if !member {
			return ErrTaskNotAssignee
		}
	case AssigneeAgent:
	default:
		return ErrTaskNotRunnable
	}
	return nil
}
