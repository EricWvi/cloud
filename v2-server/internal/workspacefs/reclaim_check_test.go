package workspacefs_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

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
	if !got.Unchanged || !got.Git.Empty() || got.Total != 0 || got.Tree == "" {
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
		want  workspacefs.GitFindings
	}{
		"two stashes": {func(t *testing.T, root string) {
			deliveryWrite(t, root, "README.md", "stashed once\n")
			deliveryGit(t, root, "stash", "push", "-q")
			deliveryWrite(t, root, "README.md", "stashed twice\n")
			deliveryGit(t, root, "stash", "push", "-q")
		}, workspacefs.GitFindings{Stashes: 2}},
		"branch beyond base and HEAD": {func(t *testing.T, root string) {
			deliveryGit(t, root, "stash", "push", "-q", "-u")
			deliveryGit(t, root, "checkout", "-q", "-b", "side")
			for _, n := range []string{"1", "2"} {
				deliveryWrite(t, root, "side"+n+".txt", n)
				deliveryGit(t, root, "add", "side"+n+".txt")
				deliveryGit(t, root, "commit", "-qm", "side "+n)
			}
			deliveryGit(t, root, "checkout", "-q", "-")
			deliveryGit(t, root, "stash", "pop", "-q")
		}, workspacefs.GitFindings{Refs: []workspacefs.RefFinding{{Name: "refs/heads/side", Commits: 2}}}},
		"ref to a blob": {func(t *testing.T, root string) {
			blob := deliveryGit(t, root, "hash-object", "-w", "README.md")
			deliveryGit(t, root, "update-ref", "refs/odd/blob", blob)
		}, workspacefs.GitFindings{Refs: []workspacefs.RefFinding{{Name: "refs/odd/blob", NotCommit: true}}}},
		"commit on HEAD and pseudo refs are fine": {func(t *testing.T, root string) {
			deliveryGit(t, root, "add", "-A")
			deliveryGit(t, root, "commit", "-qm", "agent commit")
			deliveryGit(t, root, "update-ref", "ORIG_HEAD", "HEAD~1")
		}, workspacefs.GitFindings{}},
		"staged only": {func(t *testing.T, root string) {
			deliveryWrite(t, root, "README.md", "staged only\n")
			deliveryGit(t, root, "add", "README.md")
			deliveryWrite(t, root, "README.md", "hello\nworld\n")
		}, workspacefs.GitFindings{IndexDiffers: true}},
		"committed only": {func(t *testing.T, root string) {
			deliveryWrite(t, root, "README.md", "committed only\n")
			deliveryGit(t, root, "commit", "-qm", "agent", "README.md")
			deliveryGit(t, root, "restore", "--source=HEAD~1", "--staged", "--worktree", "README.md")
		}, workspacefs.GitFindings{HeadDiffers: true, HeadCommits: 1}},
		"intent to add and unstaged edits are fine": {func(t *testing.T, root string) {
			deliveryWrite(t, root, "later.txt", "later\n")
			deliveryGit(t, root, "add", "-N", "later.txt")
		}, workspacefs.GitFindings{}},
		"gitlink": {func(t *testing.T, root string) {
			sub := filepath.Join(root, "vendor", "lib")
			deliveryWrite(t, sub, "lib.go", "package lib\n")
			deliveryGit(t, sub, "init", "-q", ".")
			deliveryGit(t, sub, "add", ".")
			deliveryGit(t, sub, "commit", "-qm", "lib")
		}, workspacefs.GitFindings{Repositories: []string{"vendor/lib"}}},
		"gitfile in an ignored directory": {func(t *testing.T, root string) {
			deliveryWrite(t, root, ".gitignore", "*.log\ncache/\n")
			deliveryWrite(t, root, "cache/wt/.git", "gitdir: /elsewhere\n")
		}, workspacefs.GitFindings{Repositories: []string{"cache/wt"}}},
		"initialized submodule with changes": {func(t *testing.T, root string) {
			lib := t.TempDir()
			deliveryWrite(t, lib, "lib.go", "package lib\n")
			deliveryGit(t, lib, "init", "-q", ".")
			deliveryGit(t, lib, "add", ".")
			deliveryGit(t, lib, "commit", "-qm", "lib")
			deliveryGit(t, root, "-c", "protocol.file.allow=always", "submodule", "add", "-q", lib, "mod")
			deliveryGit(t, root, "commit", "-qm", "submodule")
			deliveryWrite(t, root, "mod/lib.go", "package lib // edited\n")
		}, workspacefs.GitFindings{Repositories: []string{"mod", ".git/modules/mod"}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
			tc.setup(t, req.Workspace.Root)
			// Compared with a delivery of the checkout as it now is, as reclaiming does.
			snap, err := deliver(t, f, req)
			if err != nil {
				t.Fatal(err)
			}
			got := inspect(t, f, req, snap.Commit).Git
			want := tc.want
			if want.Refs == nil {
				want.Refs = []workspacefs.RefFinding{}
			}
			if want.Repositories == nil {
				want.Repositories = []string{}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("git findings = %+v, want %+v", got, want)
			}
		})
	}
}

// Ignored files and Ora's materialized skills written after a delivery are in no delivery by
// definition: they do not make the checkout differ from it.
func TestInspectReclaimIgnoresIgnoredFilesAndMaterializedSkills(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	root := req.Workspace.Root
	deliveryWrite(t, root, ".git/info/exclude", "# Skills Ora materialized for the Agent; not part of the project.\n/.claude/skills/lint\n")
	snap, err := deliver(t, f, req)
	if err != nil || snap.Commit == "" {
		t.Fatalf("deliver = %+v, %v", snap, err)
	}
	deliveryWrite(t, root, "later.log", "ignored by .gitignore\n")
	deliveryWrite(t, root, ".claude/skills/lint/SKILL.md", "# materialized\n")
	if got := inspect(t, f, req, snap.Commit); !got.Unchanged || got.Total != 0 {
		t.Fatalf("ignored or materialized writes changed the checkout: %+v", got)
	}
}

// A .git that points nowhere is broken metadata: the checkout cannot be verified (it is not taken
// as a checkout without a repository).
func TestInspectReclaimCannotVerifyAnUnresolvableGit(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	root := req.Workspace.Root
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	deliveryWrite(t, root, ".git", "gitdir: "+filepath.Join(t.TempDir(), "missing")+"\n")
	unlock, err := f.LockWorkspace(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := f.InspectReclaim(t.Context(), req.Workspace, req.MirrorPath, ""); !errors.Is(err, workspacefs.ErrUnverifiable) {
		t.Fatalf("err = %v, want ErrUnverifiable", err)
	}
}

// The identity digest stays put while nothing changes and moves with a nested repository's
// commits or working tree, which the shown findings (its path) do not reflect.
func TestInspectReclaimGitIdentityFollowsNestedRepositories(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	sub := filepath.Join(req.Workspace.Root, "vendor", "lib")
	deliveryWrite(t, sub, "lib.go", "package lib\n")
	deliveryGit(t, sub, "init", "-q", ".")
	deliveryGit(t, sub, "add", ".")
	deliveryGit(t, sub, "commit", "-qm", "lib")
	first := inspect(t, f, req, "")
	if again := inspect(t, f, req, ""); again.GitIdentity != first.GitIdentity || first.GitIdentity == "" {
		t.Fatalf("identity moved without a change: %q, %q", first.GitIdentity, again.GitIdentity)
	}
	deliveryWrite(t, sub, "lib.go", "package lib // uncommitted\n")
	edited := inspect(t, f, req, "")
	if edited.GitIdentity == first.GitIdentity || !reflect.DeepEqual(edited.Git, first.Git) {
		t.Fatalf("an uncommitted nested change kept the identity: %+v", edited)
	}
	deliveryGit(t, sub, "commit", "-qam", "lib 2")
	if committed := inspect(t, f, req, ""); committed.GitIdentity == edited.GitIdentity {
		t.Fatal("a nested commit kept the identity")
	}
}

// A FIFO where a nested repository's Git metadata belongs neither hangs the inspection nor ignores
// its context: it returns, at the latest a bounded while after the context ends.
func TestInspectReclaimDoesNotHangOnAFIFO(t *testing.T) {
	for name, place := range map[string]func(t *testing.T, root string){
		"gitfile": func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, "data"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(root, "data", ".git"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"nested stash log": func(t *testing.T, root string) {
			sub := filepath.Join(root, "vendor", "lib")
			deliveryWrite(t, sub, "lib.go", "package lib\n")
			deliveryGit(t, sub, "init", "-q", ".")
			deliveryGit(t, sub, "add", ".")
			deliveryGit(t, sub, "commit", "-qm", "lib")
			if err := os.MkdirAll(filepath.Join(sub, ".git", "logs", "refs"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(sub, ".git", "logs", "refs", "stash"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"nested HEAD": func(t *testing.T, root string) {
			sub := filepath.Join(root, "vendor", "lib")
			deliveryWrite(t, sub, "lib.go", "package lib\n")
			deliveryGit(t, sub, "init", "-q", ".")
			if err := os.Remove(filepath.Join(sub, ".git", "HEAD")); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(sub, ".git", "HEAD"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
			place(t, req.Workspace.Root)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			start := time.Now()
			type result struct {
				in  workspacefs.ReclaimInspection
				err error
			}
			done := make(chan result, 1)
			go func() {
				in, err := f.InspectReclaim(ctx, req.Workspace, req.MirrorPath, "")
				done <- result{in, err}
			}()
			select {
			case res := <-done:
				// Where Ora reads the metadata itself the FIFO leaves the digest incomplete at once;
				// where Git reads it first, the context ends Git.
				if res.err == nil && res.in.GitIdentityComplete {
					t.Fatal("a FIFO in Git metadata was bound as if it were content")
				}
				if name != "nested HEAD" && res.err != nil {
					t.Fatalf("err = %v, want an incomplete digest", res.err)
				}
				t.Logf("returned after %v: %v", time.Since(start), res.err)
			case <-time.After(30 * time.Second):
				// Bounded by the context (2s) and process teardown, not by a tight timing.
				t.Fatal("inspection hung on a FIFO")
			}
		})
	}
}
