package workspacefs_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/workspacefs"
)

const oraHeader = "# Skills Ora materialized for the Agent; not part of the project.\n"

func deliveredTree(t *testing.T, mirror string, snap workspacefs.Snapshot) string {
	t.Helper()
	if snap.Commit == "" {
		t.Fatal("delivery recorded no commit")
	}
	return deliveryGit(t, mirror, "ls-tree", "-r", "--name-only", snap.Commit)
}

// The Agent may delete .git (say `rm -rf .git && git init`, interrupted). Delivery needs no Agent
// repository, so the checkout still delivers. The record of Ora's materialized skills lived in the
// deleted exclude, so a checkout that changes a skill directory fails closed instead.
func TestDeliveryWithoutGitDirectory(t *testing.T) {
	t.Run("delivers ordinary changes", func(t *testing.T) {
		f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
		deliveryWrite(t, req.Workspace.Root, "after-rm.txt", "still delivered")
		if err := os.RemoveAll(filepath.Join(req.Workspace.Root, ".git")); err != nil {
			t.Fatal(err)
		}
		snap, err := deliver(t, f, req)
		if err != nil {
			t.Fatal(err)
		}
		if tree := deliveredTree(t, req.MirrorPath, snap); !strings.Contains(tree, "after-rm.txt") {
			t.Fatalf("delivery without .git lost the change: %s", tree)
		}
	})
	for _, dir := range []string{".opencode", ".claude"} {
		t.Run("fails closed on a materialized skill under "+dir, func(t *testing.T) {
			f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
			root := req.Workspace.Root
			deliveryWrite(t, root, ".git/info/exclude", oraHeader+"/"+dir+"/skills/private\n")
			deliveryWrite(t, root, dir+"/skills/private/SKILL.md", "private skill")
			deliveryWrite(t, root, "normal.txt", "change")
			if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
				t.Fatal(err)
			}
			if _, err := deliver(t, f, req); !errors.Is(err, workspacefs.ErrMaterializedUnknown) {
				t.Fatalf("delivery without the Ora exclude = %v, want ErrMaterializedUnknown", err)
			}
			if refs := deliveryGit(t, req.MirrorPath, "for-each-ref", "refs/ora/deliveries"); refs != "" {
				t.Fatalf("a refused delivery wrote a ref: %s", refs)
			}
		})
	}
	t.Run("a pointer to a missing directory is broken, not absent", func(t *testing.T) {
		f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
		root := req.Workspace.Root
		if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
			t.Fatal(err)
		}
		deliveryWrite(t, root, ".git", "gitdir: "+filepath.Join(t.TempDir(), "gone")+"\n")
		if _, err := deliver(t, f, req); err == nil || errors.Is(err, workspacefs.ErrMaterializedUnknown) {
			t.Fatalf("dangling gitdir pointer = %v, want a metadata error", err)
		}
	})
	t.Run("a removed checkout is gone", func(t *testing.T) {
		f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
		if err := os.RemoveAll(req.Workspace.Root); err != nil {
			t.Fatal(err)
		}
		if _, err := deliver(t, f, req); !errors.Is(err, workspacefs.ErrGone) {
			t.Fatalf("removed checkout = %v, want ErrGone", err)
		}
	})
}

// Git accepts .git as a symlink to the repository directory; its excludes, Ora's included, apply.
func TestDeliveryThroughGitSymlink(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	root := req.Workspace.Root
	deliveryWrite(t, root, ".git/info/exclude", oraHeader+"/.claude/skills/private\n")
	moved := filepath.Join(t.TempDir(), "repo.git")
	if err := os.Rename(filepath.Join(root, ".git"), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	deliveryWrite(t, root, ".claude/skills/private/SKILL.md", "private skill")
	deliveryWrite(t, root, "normal.txt", "change")
	snap, err := deliver(t, f, req)
	if err != nil {
		t.Fatal(err)
	}
	tree := deliveredTree(t, req.MirrorPath, snap)
	if strings.Contains(tree, "SKILL.md") || !strings.Contains(tree, "normal.txt") {
		t.Fatalf("delivery through a .git symlink: %s", tree)
	}
}
