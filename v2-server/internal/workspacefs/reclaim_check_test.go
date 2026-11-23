package workspacefs_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// HEAD fails closed: a detached HEAD or a branch whose commit cannot be read makes the checkout
// unverifiable; only an unborn branch (a ref that does not exist) has no HEAD to compare.
func TestInspectReclaimFailsClosedOnAnUnreadableHEAD(t *testing.T) {
	brokenCommit := func(t *testing.T, root string) string {
		deliveryWrite(t, root, "README.md", "only in this commit\n")
		deliveryGit(t, root, "commit", "-qm", "unique", "README.md")
		commit := deliveryGit(t, root, "rev-parse", "HEAD")
		deliveryGit(t, root, "restore", "--source=HEAD~1", "--staged", "--worktree", "README.md")
		if err := os.Remove(filepath.Join(root, ".git", "objects", commit[:2], commit[2:])); err != nil {
			t.Fatal(err)
		}
		return commit
	}
	for name, tc := range map[string]struct {
		setup        func(t *testing.T, root string)
		unverifiable bool
	}{
		"detached": {func(t *testing.T, root string) {
			deliveryGit(t, root, "checkout", "-q", "--detach")
			brokenCommit(t, root)
		}, true},
		"existing branch": {func(t *testing.T, root string) { brokenCommit(t, root) }, true},
		"unborn branch":   {func(t *testing.T, root string) { deliveryGit(t, root, "symbolic-ref", "HEAD", "refs/heads/not-yet") }, false},
	} {
		t.Run(name, func(t *testing.T) {
			f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
			tc.setup(t, req.Workspace.Root)
			unlock, err := f.LockWorkspace(t.Context(), req.Workspace.Root)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			_, err = f.InspectReclaim(t.Context(), req.Workspace, req.MirrorPath, "")
			if got := errors.Is(err, workspacefs.ErrUnverifiable); got != tc.unverifiable || (!tc.unverifiable && err != nil) {
				t.Fatalf("err = %v, want unverifiable %v", err, tc.unverifiable)
			}
		})
	}
}

// A ref Git cannot read (for-each-ref only warns and skips it) and a HEAD naming an existing alias
// that dangles are not taken for absent: the checkout is unverifiable.
func TestInspectReclaimDoesNotTrustRefsGitSkips(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, root string){
		"truncated loose ref": func(t *testing.T, root string) {
			deliveryGit(t, root, "branch", "side")
			deliveryWrite(t, root, ".git/refs/heads/side", "abcdef012345\n")
		},
		"HEAD naming a dangling alias": func(t *testing.T, root string) {
			deliveryGit(t, root, "symbolic-ref", "refs/heads/alias", "refs/heads/missing")
			deliveryGit(t, root, "symbolic-ref", "HEAD", "refs/heads/alias")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
			setup(t, req.Workspace.Root)
			unlock, err := f.LockWorkspace(t.Context(), req.Workspace.Root)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			if _, err := f.InspectReclaim(t.Context(), req.Workspace, req.MirrorPath, ""); !errors.Is(err, workspacefs.ErrUnverifiable) {
				t.Fatalf("err = %v, want ErrUnverifiable", err)
			}
		})
	}
}

// An I/O error reading HEAD's tree is not a verdict on the checkout: it is returned as is, to be
// retried, and the same inspection succeeds once the fault is gone.
func TestInspectReclaimRetriesAPassingHEADReadError(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	failed := false
	restore := workspacefs.InjectGitFault(func(args []string) error {
		if !failed && len(args) > 0 && args[0] == "ls-tree" && args[len(args)-1] == "HEAD" {
			failed = true
			return errors.New("fatal: unable to read tree: Input/output error")
		}
		return nil
	})
	defer restore()
	unlock, err := f.LockWorkspace(t.Context(), req.Workspace.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := f.InspectReclaim(t.Context(), req.Workspace, req.MirrorPath, ""); err == nil || errors.Is(err, workspacefs.ErrUnverifiable) {
		t.Fatalf("err = %v, want a retryable error", err)
	}
	if _, err := f.InspectReclaim(t.Context(), req.Workspace, req.MirrorPath, ""); err != nil {
		t.Fatalf("after the fault: %v", err)
	}
}

// A HEAD that cannot be resolved because time ran out is not a verdict on the checkout: the
// inspection returns the context's error, never ErrUnverifiable.
func TestInspectReclaimDoesNotJudgeAHEADThatTimesOut(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	defer workspacefs.InjectGitFault(func(args []string) error {
		if strings.Join(args, " ") == "rev-parse -q --verify HEAD^{commit}" {
			time.Sleep(300 * time.Millisecond) // Git hangs past the deadline and is killed
			return errors.New("signal: killed")
		}
		return nil
	})()
	unlock, err := f.LockWorkspace(t.Context(), req.Workspace.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := f.InspectReclaim(ctx, req.Workspace, req.MirrorPath, ""); err == nil || errors.Is(err, workspacefs.ErrUnverifiable) {
		t.Fatalf("err = %v, want a retryable error", err)
	}
}

// Nested Git directories containing symlinks or objects/info/alternates make the identity digest
// incomplete so a discard is refused, while symlinks in a nested repository's own working tree
// stay complete.
func TestInspectReclaimMarksSymlinksAndAlternatesInNestedGitIncomplete(t *testing.T) {
	initLib := func(t *testing.T) string {
		t.Helper()
		lib := t.TempDir()
		deliveryWrite(t, lib, "lib.go", "package lib\n")
		deliveryGit(t, lib, "init", "-q", ".")
		deliveryGit(t, lib, "add", ".")
		deliveryGit(t, lib, "commit", "-qm", "lib")
		return lib
	}
	copyDir := func(t *testing.T, src, dst string) {
		t.Helper()
		if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct {
		setup        func(t *testing.T, root string)
		wantComplete bool
		wantRepo     string
	}{
		"symlinked refs and objects inside nested .git": {
			setup: func(t *testing.T, root string) {
				deliveryWrite(t, root, ".gitignore", "*.log\nstore.git/\nwt/\n")
				lib := initLib(t)
				shared := filepath.Join(root, "store.git")
				deliveryGit(t, root, "clone", "-q", "--bare", lib, shared)
				wt := filepath.Join(root, "wt")
				if err := os.MkdirAll(filepath.Join(wt, ".git"), 0o700); err != nil {
					t.Fatal(err)
				}
				head, err := os.ReadFile(filepath.Join(shared, "HEAD"))
				if err != nil {
					t.Fatal(err)
				}
				deliveryWrite(t, wt, ".git/HEAD", string(head))
				deliveryWrite(t, wt, ".git/config", "[core]\n\tbare = false\n\tlogallrefupdates = true\n")
				for _, e := range []string{"refs", "packed-refs", "objects", "logs"} {
					if err := os.Symlink(filepath.Join("..", "..", "store.git", e), filepath.Join(wt, ".git", e)); err != nil {
						t.Fatal(err)
					}
				}
			},
			wantRepo: "wt",
		},
		"symlinked index inside nested .git": {
			setup: func(t *testing.T, root string) {
				deliveryWrite(t, root, ".gitignore", "*.log\nextra/\nwt/\n")
				wt := filepath.Join(root, "wt")
				deliveryWrite(t, wt, "lib.go", "package lib\n")
				deliveryGit(t, wt, "init", "-q", ".")
				deliveryGit(t, wt, "add", "lib.go")
				deliveryGit(t, wt, "commit", "-qm", "lib")
				deliveryWrite(t, wt, "lib.go", "package lib // staged\n")
				deliveryGit(t, wt, "add", "lib.go")
				extraIndex := filepath.Join(root, "extra", "index")
				if err := os.MkdirAll(filepath.Dir(extraIndex), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(wt, ".git", "index"), extraIndex); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join("..", "..", "extra", "index"), filepath.Join(wt, ".git", "index")); err != nil {
					t.Fatal(err)
				}
			},
			wantRepo: "wt",
		},
		"symlink inside gitfile target": {
			setup: func(t *testing.T, root string) {
				deliveryWrite(t, root, ".gitignore", "*.log\nstore.git/\nmeta/\nwt/\n")
				lib := initLib(t)
				shared := filepath.Join(root, "store.git")
				deliveryGit(t, root, "clone", "-q", "--bare", lib, shared)
				meta := filepath.Join(root, "meta", "wt")
				if err := os.MkdirAll(meta, 0o700); err != nil {
					t.Fatal(err)
				}
				head, err := os.ReadFile(filepath.Join(shared, "HEAD"))
				if err != nil {
					t.Fatal(err)
				}
				deliveryWrite(t, meta, "HEAD", string(head))
				deliveryWrite(t, meta, "config", "[core]\n\tbare = false\n")
				if err := os.Symlink(filepath.Join("..", "..", "store.git", "objects"), filepath.Join(meta, "objects")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join("..", "..", "store.git", "refs"), filepath.Join(meta, "refs")); err != nil {
					t.Fatal(err)
				}
				deliveryWrite(t, root, "wt/.git", "gitdir: ../meta/wt\n")
			},
			wantRepo: "wt",
		},
		"symlink inside commondir target": {
			setup: func(t *testing.T, root string) {
				deliveryWrite(t, root, ".gitignore", "*.log\nstore.git/\nother-refs/\nwt/\n")
				lib := initLib(t)
				shared := filepath.Join(root, "store.git")
				deliveryGit(t, root, "clone", "-q", "--bare", lib, shared)
				wt := filepath.Join(root, "wt")
				deliveryGit(t, shared, "worktree", "add", "-q", "--detach", wt)
				if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(shared, "worktrees", "wt"), filepath.Join(wt, ".git")); err != nil {
					t.Fatal(err)
				}
				deliveryWrite(t, wt, ".git/commondir", "../../store.git\n")
				otherRefs := filepath.Join(root, "other-refs")
				copyDir(t, filepath.Join(shared, "refs"), otherRefs)
				if err := os.RemoveAll(filepath.Join(shared, "refs")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join("..", "other-refs"), filepath.Join(shared, "refs")); err != nil {
					t.Fatal(err)
				}
			},
			wantRepo: "wt",
		},
		"symlink inside .git/modules entry": {
			setup: func(t *testing.T, root string) {
				deliveryWrite(t, root, ".gitignore", "*.log\nstore.git/\n")
				lib := initLib(t)
				shared := filepath.Join(root, "store.git")
				deliveryGit(t, root, "clone", "-q", "--bare", lib, shared)
				deliveryGit(t, root, "-c", "protocol.file.allow=always", "submodule", "add", "-q", lib, "mod")
				deliveryGit(t, root, "commit", "-qm", "submodule")
				modObjects := filepath.Join(root, ".git", "modules", "mod", "objects")
				if err := os.RemoveAll(modObjects); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join("..", "..", "..", "store.git", "objects"), modObjects); err != nil {
					t.Fatal(err)
				}
			},
			wantRepo: ".git/modules/mod",
		},
		"symlinked .git/modules entry": {
			setup: func(t *testing.T, root string) {
				deliveryWrite(t, root, ".gitignore", "*.log\n")
				lib := initLib(t)
				deliveryGit(t, root, "-c", "protocol.file.allow=always", "submodule", "add", "-q", lib, "mod")
				deliveryGit(t, root, "commit", "-qm", "submodule")
				if err := os.RemoveAll(filepath.Join(root, "mod")); err != nil {
					t.Fatal(err)
				}
				realDir := filepath.Join(root, ".git", "modules", "real", "mod")
				if err := os.MkdirAll(filepath.Dir(realDir), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(root, ".git", "modules", "mod"), realDir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join("real", "mod"), filepath.Join(root, ".git", "modules", "mod")); err != nil {
					t.Fatal(err)
				}
			},
			wantRepo: ".git/modules/mod",
		},
		"commondir inside .git/modules entry": {
			setup: func(t *testing.T, root string) {
				deliveryWrite(t, root, ".gitignore", "*.log\nstore.git/\n")
				lib := initLib(t)
				shared := filepath.Join(root, "store.git")
				deliveryGit(t, root, "clone", "-q", "--bare", lib, shared)
				deliveryGit(t, root, "-c", "protocol.file.allow=always", "submodule", "add", "-q", lib, "mod")
				deliveryGit(t, root, "commit", "-qm", "submodule")
				if err := os.RemoveAll(filepath.Join(root, "mod")); err != nil {
					t.Fatal(err)
				}
				deliveryWrite(t, root, ".git/modules/mod/commondir", "../../store.git\n")
			},
			wantRepo: ".git/modules/mod",
		},
		"alternates inside nested .git": {
			setup: func(t *testing.T, root string) {
				deliveryWrite(t, root, ".gitignore", "*.log\nstore.git/\nwt/\n")
				lib := initLib(t)
				shared := filepath.Join(root, "store.git")
				deliveryGit(t, root, "clone", "-q", "--bare", lib, shared)
				wt := filepath.Join(root, "wt")
				deliveryWrite(t, wt, "lib.go", "package lib\n")
				deliveryGit(t, wt, "init", "-q", ".")
				deliveryGit(t, wt, "add", ".")
				deliveryGit(t, wt, "commit", "-qm", "lib")
				deliveryWrite(t, wt, ".git/objects/info/alternates", "../../../store.git/objects\n")
			},
			wantRepo: "wt",
		},
		"alternates deeper inside a nested Git directory": {
			setup: func(t *testing.T, root string) {
				deliveryWrite(t, root, ".gitignore", "*.log\nstore.git/\nwt/\n")
				lib := initLib(t)
				shared := filepath.Join(root, "store.git")
				deliveryGit(t, root, "clone", "-q", "--bare", lib, shared)
				wt := filepath.Join(root, "wt")
				deliveryWrite(t, wt, "lib.go", "package lib\n")
				deliveryGit(t, wt, "init", "-q", ".")
				deliveryGit(t, wt, "add", ".")
				deliveryGit(t, wt, "commit", "-qm", "lib")
				deliveryGit(t, wt, "-c", "protocol.file.allow=always", "submodule", "add", "-q", lib, "mod")
				deliveryGit(t, wt, "commit", "-qm", "submodule")
				// Deinit: the submodule has no working tree, but its Git directory stays.
				if err := os.RemoveAll(filepath.Join(wt, "mod")); err != nil {
					t.Fatal(err)
				}
				deliveryWrite(t, wt, ".git/modules/mod/objects/info/alternates", "../../../../store.git/objects\n")
			},
			wantRepo: "wt",
		},
		"nested Git directory resolving to the checkout root": {
			setup: func(t *testing.T, root string) {
				deliveryWrite(t, root, ".gitignore", "*.log\nwt/\n")
				wt := filepath.Join(root, "wt")
				deliveryWrite(t, wt, "lib.go", "package lib\n")
				deliveryGit(t, wt, "init", "-q", ".")
				deliveryGit(t, wt, "add", ".")
				deliveryGit(t, wt, "commit", "-qm", "lib")
				if err := os.RemoveAll(filepath.Join(wt, ".git")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("..", filepath.Join(wt, ".git")); err != nil {
					t.Fatal(err)
				}
			},
			wantRepo: "wt",
		},
		"alternates inside .git/modules entry": {
			setup: func(t *testing.T, root string) {
				deliveryWrite(t, root, ".gitignore", "*.log\nstore.git/\n")
				lib := initLib(t)
				shared := filepath.Join(root, "store.git")
				deliveryGit(t, root, "clone", "-q", "--bare", lib, shared)
				deliveryGit(t, root, "-c", "protocol.file.allow=always", "submodule", "add", "-q", lib, "mod")
				deliveryGit(t, root, "commit", "-qm", "submodule")
				deliveryWrite(t, root, ".git/modules/mod/objects/info/alternates", "../../../../store.git/objects\n")
			},
			wantRepo: ".git/modules/mod",
		},
		"working tree symlink stays complete": {
			setup: func(t *testing.T, root string) {
				sub := filepath.Join(root, "vendor", "lib")
				deliveryWrite(t, sub, "lib.go", "package lib\n")
				deliveryGit(t, sub, "init", "-q", ".")
				deliveryGit(t, sub, "add", ".")
				deliveryGit(t, sub, "commit", "-qm", "lib")
				if err := os.Symlink("lib.go", filepath.Join(sub, "link.go")); err != nil {
					t.Fatal(err)
				}
			},
			wantComplete: true,
			wantRepo:     "vendor/lib",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
			tc.setup(t, req.Workspace.Root)
			got := inspect(t, f, req, "")
			if got.GitIdentityComplete != tc.wantComplete {
				t.Fatalf("GitIdentityComplete = %v, want %v (git=%+v)", got.GitIdentityComplete, tc.wantComplete, got.Git)
			}
			found := false
			for _, repo := range got.Git.Repositories {
				if repo == tc.wantRepo {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("Git.Repositories = %v, want %q", got.Git.Repositories, tc.wantRepo)
			}
		})
	}
}
