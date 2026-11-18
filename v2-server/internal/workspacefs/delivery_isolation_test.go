package workspacefs_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/workspacefs"
)

// sentinel returns a script path that creates marker when executed, plus the marker path.
func gitCodeSentinel(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	marker := filepath.Join(dir, name+".ran")
	script := filepath.Join(dir, name+".sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\ncat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, marker
}

func TestDeliveryNoForeignGitCodeRuns(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	root := req.Workspace.Root
	side := t.TempDir()
	repoFilter, repoMarker := gitCodeSentinel(t, side, "repo-filter")
	globalFilter, globalMarker := gitCodeSentinel(t, side, "global-filter")
	envFilter, envMarker := gitCodeSentinel(t, side, "env-filter")
	fsmon, fsmonMarker := gitCodeSentinel(t, side, "fsmonitor")
	hook, hookMarker := gitCodeSentinel(t, side, "hook")
	textconv, textconvMarker := gitCodeSentinel(t, side, "textconv")

	// Stash and reflog state must survive.
	deliveryWrite(t, root, "README.md", "stashed\n")
	deliveryGit(t, root, "stash", "push", "-q")
	// Agent repository: filter for every path via .gitattributes and .git/info/attributes,
	// fsmonitor hook, hooks path, textconv diff driver.
	deliveryWrite(t, root, ".gitattributes", "* filter=evil diff=evil\n")
	deliveryWrite(t, root, ".git/info/attributes", "* filter=evil diff=evil\n")
	deliveryGit(t, root, "config", "filter.evil.clean", repoFilter)
	deliveryGit(t, root, "config", "filter.evil.required", "true")
	deliveryGit(t, root, "config", "core.fsmonitor", fsmon)
	deliveryGit(t, root, "config", "core.hooksPath", side)
	deliveryGit(t, root, "config", "diff.evil.textconv", textconv)
	for _, h := range []string{"pre-commit", "post-commit", "post-index-change", "reference-transaction", "post-checkout"} {
		if err := os.Symlink(hook, filepath.Join(side, h)); err != nil {
			t.Fatal(err)
		}
	}
	// Global configuration through HOME, XDG and GIT_CONFIG_GLOBAL, plus env-injected config.
	home := t.TempDir()
	global := fmt.Sprintf("[filter \"evil\"]\n\tclean = %s\n\trequired = true\n[core]\n\tfsmonitor = %s\n\thooksPath = %s\n\tattributesFile = %s\n[diff \"evil\"]\n\ttextconv = %s\n",
		globalFilter, fsmon, side, filepath.Join(home, "attrs"), textconv)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "attrs"), []byte("* filter=evil diff=evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "filter.evil.clean")
	t.Setenv("GIT_CONFIG_VALUE_0", envFilter)
	t.Setenv("GIT_CONFIG_PARAMETERS", "'filter.evil.clean'='"+envFilter+"'")

	deliveryWrite(t, root, "README.md", "delivered\n")
	// Symlinks pointing outside the checkout, including a symlinked directory.
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "leak")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(root, "etcdir")); err != nil {
		t.Fatal(err)
	}
	read := func(rel string) []byte {
		b, _ := os.ReadFile(filepath.Join(root, rel))
		return b
	}
	idx := read(".git/index")
	stash := deliveryGit(t, root, "-c", "core.fsmonitor=false", "stash", "list")
	reflog := deliveryGit(t, root, "-c", "core.fsmonitor=false", "reflog", "-n", "5")
	refs := deliveryGit(t, root, "-c", "core.fsmonitor=false", "show-ref")
	// Remove markers possibly created by the fixture's own git calls above.
	for _, m := range []string{repoMarker, globalMarker, envMarker, fsmonMarker, hookMarker, textconvMarker} {
		_ = os.Remove(m)
	}

	snap, err := deliver(t, f, req)
	if err != nil {
		t.Fatal(err)
	}
	ds := workspacefs.DeliverySnapshot{MirrorPath: req.MirrorPath, ProjectID: req.ProjectID, DeliveryID: req.DeliveryID, BaseCommit: req.Workspace.BaseCommit, Commit: snap.Commit}
	if _, err := f.DeliveryChanges(t.Context(), ds); err != nil {
		t.Fatal(err)
	}
	if _, err := f.DeliveryDiff(t.Context(), ds, "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.DeliveryPatch(t.Context(), ds, 1<<20); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{repoMarker, globalMarker, envMarker, fsmonMarker, hookMarker, textconvMarker} {
		if _, err := os.Stat(m); err == nil {
			t.Errorf("foreign Git code ran: %s", filepath.Base(m))
		}
	}
	// Content captured raw, symlinks recorded as links.
	if got := deliveryGit(t, req.MirrorPath, "cat-file", "-p", snap.Commit+":README.md"); got != "delivered" {
		t.Errorf("README content = %q", got)
	}
	if got := deliveryGit(t, req.MirrorPath, "cat-file", "-p", snap.Commit+":leak"); got != "/etc/passwd" {
		t.Errorf("leak symlink content = %q", got)
	}
	tree := deliveryGit(t, req.MirrorPath, "ls-tree", snap.Commit)
	if !strings.Contains(tree, "120000 blob") || strings.Contains(deliveryGit(t, req.MirrorPath, "ls-tree", "-r", "--name-only", snap.Commit), "etcdir/") {
		t.Errorf("symlink handling: %s", tree)
	}
	if !bytes.Equal(idx, read(".git/index")) || stash != deliveryGit(t, root, "-c", "core.fsmonitor=false", "stash", "list") ||
		reflog != deliveryGit(t, root, "-c", "core.fsmonitor=false", "reflog", "-n", "5") || refs != deliveryGit(t, root, "-c", "core.fsmonitor=false", "show-ref") {
		t.Error("Agent repository state changed")
	}
}

func TestDeliveryExcludeSymlinkOutside(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	root := req.Workspace.Root
	outside := filepath.Join(t.TempDir(), "exclude")
	if err := os.WriteFile(outside, []byte("README.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(root, ".git/info/exclude"))
	if err := os.MkdirAll(filepath.Join(root, ".git/info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".git/info/exclude")); err != nil {
		t.Fatal(err)
	}
	snap, err := deliver(t, f, req)
	if err == nil || snap.Commit != "" {
		t.Fatalf("escaping exclude symlink did not fail closed: %+v, %v", snap, err)
	}
	if refs := deliveryGit(t, req.MirrorPath, "for-each-ref", "refs/ora/deliveries"); refs != "" {
		t.Fatalf("escaping exclude published a delivery: %s", refs)
	}
}

func TestDeliveryGitFileExcludesMaterializedSkill(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	root := req.Workspace.Root
	gitdir := filepath.Join(t.TempDir(), "gitdir")
	if err := os.Rename(filepath.Join(root, ".git"), gitdir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// What the materializer would have recorded had .git been a directory:
	deliveryWrite(t, gitdir, "info/exclude", "# Skills Ora materialized for the Agent; not part of the project.\n/.agents/skills/private\n")
	deliveryWrite(t, root, ".agents/skills/private/SKILL.md", "private user skill")
	snap, err := deliver(t, f, req)
	if err != nil {
		t.Fatal(err)
	}
	tree := deliveryGit(t, req.MirrorPath, "ls-tree", "-r", "--name-only", snap.Commit)
	if strings.Contains(tree, ".agents/skills/private/SKILL.md") {
		t.Errorf("materialized skill delivered for a gitfile checkout:\n%s", tree)
	}
}

func TestDeliverySymlinkedExcludeKeepsMaterializedSkillPrivate(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	root := req.Workspace.Root
	deliveryWrite(t, root, ".git/ora-exclude", "# Skills Ora materialized for the Agent; not part of the project.\n/.agents/skills/private\n")
	_ = os.Remove(filepath.Join(root, ".git/info/exclude"))
	if err := os.MkdirAll(filepath.Join(root, ".git/info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../ora-exclude", filepath.Join(root, ".git/info/exclude")); err != nil {
		t.Fatal(err)
	}
	deliveryWrite(t, root, ".agents/skills/private/SKILL.md", "private user skill")
	snap, err := deliver(t, f, req)
	if err != nil {
		t.Fatal(err)
	}
	tree := deliveryGit(t, req.MirrorPath, "ls-tree", "-r", "--name-only", snap.Commit)
	if strings.Contains(tree, ".agents/skills/private/SKILL.md") {
		t.Errorf("materialized skill delivered through a symlinked exclude:\n%s", tree)
	}
}

func TestDeliveryTrackedFileNewlyIgnored(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	root := req.Workspace.Root
	deliveryWrite(t, root, ".gitignore", "README.md\n")
	deliveryWrite(t, root, "README.md", "modified while ignored\n")
	snap, err := deliver(t, f, req)
	if err != nil {
		t.Fatal(err)
	}
	if got := deliveryGit(t, req.MirrorPath, "ls-tree", "-r", "--name-only", snap.Commit); !strings.Contains(got, "README.md") {
		t.Fatalf("tracked file dropped after a new ignore rule: %s", got)
	}
	if got := deliveryGit(t, req.MirrorPath, "show", snap.Commit+":README.md"); got != "modified while ignored" {
		t.Fatalf("tracked file did not retain its modification: %q", got)
	}
}

func TestDeliveryRejectsSparseFileBeforeWritingObjects(t *testing.T) {
	_, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	file, err := os.Create(filepath.Join(req.Workspace.Root, "sparse"))
	if err != nil {
		t.Fatal(err)
	}
	err = file.Truncate(8 << 20)
	if closeErr := file.Close(); err != nil || closeErr != nil {
		t.Fatalf("create sparse input: %v, close: %v", err, closeErr)
	}

	assertDeliveryPreflightRejectsBeforeGitAdd(t, req, 4096)
}

func TestDeliveryRejectsAggregateInputBeforeWritingObjects(t *testing.T) {
	_, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	for i := range 8 {
		deliveryWrite(t, req.Workspace.Root, fmt.Sprintf("input-%d", i), strings.Repeat(string(rune('a'+i)), 1024))
	}
	assertDeliveryPreflightRejectsBeforeGitAdd(t, req, 4096)
}

func assertDeliveryPreflightRejectsBeforeGitAdd(t *testing.T, req workspacefs.DeliveryRequest, limit int64) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker := filepath.Join(bin, "add-called")
	script := fmt.Sprintf("#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = add ]; then touch %q; fi\ndone\nexec %q \"$@\"\n", marker, git)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	scratch := t.TempDir()
	fs, err := workspacefs.NewDeliveryFS(scratch, limit)
	if err != nil {
		t.Fatal(err)
	}
	before := deliveryGit(t, req.MirrorPath, "count-objects", "-v")
	_, err = deliver(t, fs, req)
	if _, statErr := os.Lstat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("git add ran before rejecting oversized input: %v", statErr)
	}
	if !errors.Is(err, workspacefs.ErrDeliveryTooLarge) {
		t.Fatalf("oversized input: %v", err)
	}
	if entries, err := os.ReadDir(scratch); err != nil || len(entries) != 0 {
		t.Fatalf("scratch retained objects: %v, %v", entries, err)
	}
	if after := deliveryGit(t, req.MirrorPath, "count-objects", "-v"); after != before {
		t.Fatalf("mirror objects changed: before %q, after %q", before, after)
	}
}

func TestDeliveryKeepsLogicalObjectLimitAfterPreflight(t *testing.T) {
	fs, req := deliveryFixture(t, 64)
	deliveryGit(t, req.Workspace.Root, "reset", "--hard", req.Workspace.BaseCommit)
	deliveryGit(t, req.Workspace.Root, "clean", "-fdx")
	// One byte passes the input check, but the resulting commit and tree exceed the cap.
	deliveryWrite(t, req.Workspace.Root, "small", "x")
	before := deliveryGit(t, req.MirrorPath, "count-objects", "-v")
	if _, err := deliver(t, fs, req); !errors.Is(err, workspacefs.ErrDeliveryTooLarge) {
		t.Fatalf("logical object budget did not reject the snapshot: %v", err)
	}
	if after := deliveryGit(t, req.MirrorPath, "count-objects", "-v"); after != before {
		t.Fatalf("mirror objects changed: before %q, after %q", before, after)
	}
}

func TestDeliveryLinkedWorktreeUsesBothExcludeFiles(t *testing.T) {
	f, req := deliveryFixture(t, workspacefs.DefaultDeliveryObjectBytes)
	main := req.Workspace.Root
	root := filepath.Join(t.TempDir(), "linked")
	deliveryGit(t, main, "worktree", "add", "--detach", root, req.Workspace.BaseCommit)
	gitdir := deliveryGit(t, root, "rev-parse", "--absolute-git-dir")
	req.Workspace.Root = root

	const header = "# Skills Ora materialized for the Agent; not part of the project.\n"
	deliveryWrite(t, gitdir, "info/exclude", header+"/.agents/skills/local\n")
	deliveryWrite(t, main, ".git/info/exclude", header+"/.agents/skills/shared\n/ignored-by-common\n")
	deliveryWrite(t, root, ".agents/skills/local/SKILL.md", "local private skill")
	deliveryWrite(t, root, ".agents/skills/shared/SKILL.md", "shared private skill")
	deliveryWrite(t, root, "ignored-by-common", "project exclude")
	// Git's higher-priority .gitignore can re-include these paths; the Ora rules still win.
	deliveryWrite(t, root, ".gitignore", "!/.agents/skills/local\n!/.agents/skills/shared\n")
	deliveryWrite(t, root, "normal-change", "deliver me")

	snap, err := deliver(t, f, req)
	if err != nil {
		t.Fatal(err)
	}
	tree := deliveryGit(t, req.MirrorPath, "ls-tree", "-r", "--name-only", snap.Commit)
	if strings.Contains(tree, "SKILL.md") || strings.Contains(tree, "ignored-by-common") || !strings.Contains(tree, "normal-change") {
		t.Fatalf("linked checkout excludes were not applied: %s", tree)
	}
}
