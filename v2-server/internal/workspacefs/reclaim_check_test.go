package workspacefs_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/workspacefs"
)

func inspect(t *testing.T, f *workspacefs.DeliveryFS, req workspacefs.DeliveryRequest, against string) workspacefs.ReclaimInspection {
	t.Helper()
	unlock, err := f.LockWorkspace(t.Context(), req.Workspace.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	got, err := f.InspectReclaim(t.Context(), req.Workspace, req.MirrorPath, against)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestInspectReclaimComparesWithTheDeliveryWithoutWritingTheMirror(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	snap, err := deliver(t, f, req)
	if err != nil || snap.Commit == "" {
		t.Fatalf("deliver = %+v, %v", snap, err)
	}
	refsBefore := deliveryGit(t, req.MirrorPath, "for-each-ref")
	got := inspect(t, f, req, snap.Commit)
	if !got.Unchanged || got.GitState != "" || got.Total != 0 {
		t.Fatalf("right after delivery: %+v", got)
	}
	// debug.log is ignored by .gitignore: it is in no delivery, and the person is told.
	if !got.Ignored {
		t.Fatal("ignored file not reported")
	}
	// Against the base the same checkout has changes.
	if base := inspect(t, f, req, ""); base.Unchanged || base.Total == 0 {
		t.Fatalf("against base: %+v", base)
	}
	// A file written after the delivery makes the checkout differ from it.
	deliveryWrite(t, req.Workspace.Root, "late.txt", "after the delivery\n")
	got = inspect(t, f, req, snap.Commit)
	if got.Unchanged || got.Total != 1 || len(got.Changes) != 1 || got.Changes[0].Path != "late.txt" || got.Changes[0].Status != workspacefs.StatusAdded {
		t.Fatalf("after a late write: %+v", got)
	}
	if after := deliveryGit(t, req.MirrorPath, "for-each-ref"); after != refsBefore {
		t.Fatalf("inspection changed the mirror's refs:\n%s\n%s", refsBefore, after)
	}
}

func TestInspectReclaimWithoutIgnoredFiles(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	if err := os.Remove(filepath.Join(req.Workspace.Root, "debug.log")); err != nil {
		t.Fatal(err)
	}
	if got := inspect(t, f, req, ""); got.Ignored {
		t.Fatalf("ignored reported without ignored files: %+v", got)
	}
}

func TestInspectReclaimFindsWorkOnlyTheRepositoryHolds(t *testing.T) {
	cases := map[string]struct {
		setup func(t *testing.T, root string)
		want  string
	}{
		"stash": {func(t *testing.T, root string) {
			deliveryWrite(t, root, "README.md", "stashed\n")
			deliveryGit(t, root, "stash", "push", "-q")
		}, workspacefs.GitStateStash},
		"branch beyond base and HEAD": {func(t *testing.T, root string) {
			deliveryGit(t, root, "checkout", "-q", "-b", "side")
			deliveryWrite(t, root, "side.txt", "only on side\n")
			deliveryGit(t, root, "add", "side.txt")
			deliveryGit(t, root, "commit", "-qm", "side")
			deliveryGit(t, root, "checkout", "-q", "-")
		}, workspacefs.GitStateRef},
		"commit on HEAD is fine": {func(t *testing.T, root string) {
			deliveryGit(t, root, "add", "-A")
			deliveryGit(t, root, "commit", "-qm", "agent commit")
		}, ""},
		"gitlink": {func(t *testing.T, root string) {
			sub := filepath.Join(root, "vendor", "lib")
			deliveryWrite(t, sub, "lib.go", "package lib\n")
			deliveryGit(t, sub, "init", "-q", ".")
			deliveryGit(t, sub, "add", ".")
			deliveryGit(t, sub, "commit", "-qm", "lib")
		}, workspacefs.GitStateGitlink},
		"nested .git": {func(t *testing.T, root string) {
			deliveryWrite(t, root, "data/.git", "gitdir: /nowhere\n")
		}, workspacefs.GitStateNestedGit},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
			tc.setup(t, req.Workspace.Root)
			if got := inspect(t, f, req, ""); got.GitState != tc.want {
				t.Fatalf("git state = %q, want %q", got.GitState, tc.want)
			}
		})
	}
}
