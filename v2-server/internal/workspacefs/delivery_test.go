package workspacefs_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/workspacefs"
)

func deliveryGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Agent", "-c", "user.email=agent@example.test"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func deliveryFixture(t *testing.T, cap int64) (*workspacefs.DeliveryFS, workspacefs.DeliveryRequest) {
	t.Helper()
	w := checkout(t)
	dir := t.TempDir()
	mirror := filepath.Join(dir, "mirror.git")
	deliveryGit(t, dir, "clone", "--mirror", w.Root, mirror)
	f, err := workspacefs.NewDeliveryFS(dir, cap)
	if err != nil {
		t.Fatal(err)
	}
	return f, workspacefs.DeliveryRequest{
		Workspace: w, MirrorPath: mirror, Message: "Ora delivery: chat first turn",
		ProjectID: "faec9107-4935-49a8-aacb-e22046e995ed", DeliveryID: "ee630b8f-3738-43e5-aa40-3d29ce449c65",
	}
}

func deliver(t *testing.T, f *workspacefs.DeliveryFS, req workspacefs.DeliveryRequest) (workspacefs.Snapshot, error) {
	t.Helper()
	unlock, err := f.LockWorkspace(t.Context(), req.Workspace.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	return f.Deliver(t.Context(), req)
}

func deliveryWrite(t *testing.T, root, path, value string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryUsesBaseAndLeavesAgentRepositoryIntact(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	root := req.Workspace.Root
	deliveryGit(t, root, "add", "README.md")
	deliveryGit(t, root, "commit", "-qm", "Agent commit")
	deliveryGit(t, root, "reset", "--mixed", req.Workspace.BaseCommit)
	deliveryWrite(t, root, "src/keep.go", "staged only")
	deliveryGit(t, root, "add", "src/keep.go")
	deliveryWrite(t, root, "src/keep.go", "package src\n")
	deliveryWrite(t, root, ".git/info/exclude", "# Skills Ora materialized for the Agent; not part of the project.\n/.agents/skills/demo\n")
	deliveryWrite(t, root, ".agents/skills/demo/SKILL.md", "materialized")
	deliveryWrite(t, root, ".gitignore", "*.log\n!/.agents/skills/demo/\n!/.agents/skills/demo/SKILL.md\n")
	deliveryWrite(t, root, ".gitattributes", "*.bin filter=tripwire\n")
	deliveryGit(t, root, "config", "filter.tripwire.clean", "touch "+filepath.Join(root, "filter-ran"))
	deliveryGit(t, root, "config", "filter.tripwire.required", "true")

	read := func(rel string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	index := read(".git/index")
	head, refs := deliveryGit(t, root, "rev-parse", "HEAD"), deliveryGit(t, root, "show-ref")
	snap, err := deliver(t, f, req)
	if err != nil {
		t.Fatal(err)
	}
	if got := deliveryGit(t, req.MirrorPath, "show", "-s", "--format=%P%n%an <%ae>", snap.Commit); got != req.Workspace.BaseCommit+"\nOra <ora@ora.invalid>" {
		t.Fatalf("parent or identity = %q", got)
	}
	got := deliveryGit(t, req.MirrorPath, "ls-tree", "-r", "--name-only", snap.Commit)
	for _, path := range []string{"README.md", "logo.bin", "src/new file.go", "src/keep.go"} {
		if !strings.Contains(got, path) {
			t.Errorf("snapshot omitted %s: %s", path, got)
		}
	}
	for _, path := range []string{"debug.log", "SKILL.md", "src/old.go", "filter-ran"} {
		if strings.Contains(got, path) {
			t.Errorf("snapshot captured %s: %s", path, got)
		}
	}
	if !bytes.Equal(index, read(".git/index")) || head != deliveryGit(t, root, "rev-parse", "HEAD") ||
		refs != deliveryGit(t, root, "show-ref") || string(read("src/keep.go")) != "package src\n" {
		t.Fatal("delivery modified the Agent repository")
	}
	if _, err := os.Stat(filepath.Join(root, "filter-ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("filter ran: %v", err)
	}
}

func TestEmptyDeliveryWritesNoRef(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	deliveryGit(t, req.Workspace.Root, "reset", "--hard", req.Workspace.BaseCommit)
	deliveryGit(t, req.Workspace.Root, "clean", "-fd")
	got, err := deliver(t, f, req)
	if err != nil || got.Commit != "" || deliveryGit(t, req.MirrorPath, "for-each-ref", "refs/ora/deliveries") != "" {
		t.Fatalf("empty delivery wrote a snapshot: %+v, %v", got, err)
	}
}

func TestDeliveryObjectCap(t *testing.T) {
	f, req := deliveryFixture(t, 1)
	before := deliveryGit(t, req.MirrorPath, "count-objects", "-v")
	_, err := deliver(t, f, req)
	if !errors.Is(err, workspacefs.ErrDeliveryTooLarge) ||
		deliveryGit(t, req.MirrorPath, "for-each-ref", "refs/ora/deliveries") != "" ||
		deliveryGit(t, req.MirrorPath, "count-objects", "-v") != before {
		t.Fatalf("oversize delivery changed mirror: %v", err)
	}
}

func TestDeliveryRefIsCreateOnly(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	first, err := deliver(t, f, req)
	if err != nil {
		t.Fatal(err)
	}
	deliveryWrite(t, req.Workspace.Root, "README.md", "another delivery")
	if _, err := deliver(t, f, req); err == nil {
		t.Fatal("second commit claiming the same delivery ID succeeded")
	}
	ref := "refs/ora/deliveries/" + req.ProjectID + "/" + req.DeliveryID
	if got := deliveryGit(t, req.MirrorPath, "rev-parse", ref); got != first.Commit {
		t.Fatalf("delivery ref moved to %s", got)
	}
}

func TestDeliveryPatchAppliesOnBase(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	snapshot, err := deliver(t, f, req)
	if err != nil {
		t.Fatal(err)
	}
	snap := workspacefs.DeliverySnapshot{
		MirrorPath: req.MirrorPath, ProjectID: req.ProjectID, DeliveryID: req.DeliveryID,
		BaseCommit: req.Workspace.BaseCommit, Commit: snapshot.Commit,
	}
	if err := os.RemoveAll(req.Workspace.Root); err != nil {
		t.Fatal(err)
	}
	changes, err := f.DeliveryChanges(t.Context(), snap)
	if err != nil || len(changes.Files) != 4 {
		t.Fatalf("mirror changes: %+v, %v", changes, err)
	}
	diff, err := f.DeliveryDiff(t.Context(), snap, "src/new file.go")
	if err != nil || !strings.Contains(diff.Patch, "+func New() {}") {
		t.Fatalf("mirror diff: %+v, %v", diff, err)
	}
	patch, err := f.DeliveryPatch(t.Context(), snap, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := f.DeliveryPatch(t.Context(), snap, len(patch)-1); b != nil || !errors.Is(err, workspacefs.ErrTooLarge) {
		t.Fatalf("oversize patch: %d bytes, %v", len(b), err)
	}
	if b, err := f.DeliveryPatch(t.Context(), snap, len(patch)); err != nil || !bytes.Equal(b, patch) {
		t.Fatalf("exact patch cap: %d bytes, %v", len(b), err)
	}
	dir := t.TempDir()
	deliveryGit(t, dir, "clone", "--no-local", "--single-branch", req.MirrorPath, "base")
	base := filepath.Join(dir, "base")
	deliveryGit(t, base, "checkout", req.Workspace.BaseCommit)
	binaryOID := deliveryGit(t, req.MirrorPath, "rev-parse", snapshot.Commit+":logo.bin")
	if err := exec.Command("git", "-C", base, "cat-file", "-e", binaryOID).Run(); err == nil {
		t.Fatal("base checkout unexpectedly contains the delivery blob")
	}
	deliveryWrite(t, dir, "delivery.patch", string(patch))
	deliveryGit(t, base, "apply", filepath.Join(dir, "delivery.patch"))
	deliveryGit(t, base, "add", "-A")
	if want, got := deliveryGit(t, req.MirrorPath, "rev-parse", snapshot.Commit+"^{tree}"), deliveryGit(t, base, "write-tree"); got != want {
		t.Fatalf("applied patch tree = %s, want %s", got, want)
	}
}

func TestDeliveryWorkspaceSerialization(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	unlock, err := f.LockWorkspace(t.Context(), req.Workspace.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(req.Workspace.Root, alias); err != nil {
		t.Fatal(err)
	}
	if release, err := f.LockWorkspace(ctx, alias); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("same canonical directory admitted concurrently: %v", err)
	}
	other, err := f.LockWorkspace(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other()
}
