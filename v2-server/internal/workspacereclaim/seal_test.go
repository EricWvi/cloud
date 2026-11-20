package workspacereclaim

import (
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// Discarding is offered only for an undelivered checkout that was inspected completely: every
// combination with an incomplete or missing digest, or with nothing to discard, refuses it.
func TestSealOffersDiscardOnlyWithACompleteDigest(t *testing.T) {
	verdicts := []Verdict{
		{Workspace: WorkspaceNone, Reason: ReasonNoWorkspace},
		{Workspace: WorkspaceNone, Reason: ReasonMissing},
		{Workspace: WorkspaceRecoverable},
		{Workspace: WorkspaceUndelivered, Reason: ReasonDelivering},
		{Workspace: WorkspaceUndelivered, Reason: ReasonLegacy},
		{Workspace: WorkspaceUndelivered, Reason: ReasonLastUndelivered},
		{Workspace: WorkspaceUndelivered, Reason: ReasonGitState},
		{Workspace: WorkspaceUndelivered, Reason: ReasonChanged},
		{Workspace: WorkspaceUndelivered, Reason: ReasonUnverifiable},
	}
	for _, v := range verdicts {
		for _, content := range []string{"", "tree\x00git\x00identity"} {
			for _, complete := range []bool{false, true} {
				// An incoming Discardable must never survive: seal decides alone.
				for _, incoming := range []bool{false, true} {
					in := v
					in.Discardable = incoming
					got := seal(in, store.Checkout{}, content, complete).Discardable
					want := v.Workspace == WorkspaceUndelivered && v.Reason != ReasonDelivering && content != "" && complete
					if got != want {
						t.Errorf("%s/%s digest=%v complete=%v incoming=%v: discardable %v, want %v",
							v.Workspace, v.Reason, content != "", complete, incoming, got, want)
					}
					if !complete && got {
						t.Errorf("an incomplete digest allowed a discard: %+v", in)
					}
				}
			}
		}
	}
}
