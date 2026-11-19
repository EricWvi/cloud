package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// checkoutRepo is a git checkout of files, committed, for the deliverable checks, with a bare
// clone standing in for the project's mirror.
type checkoutRepo struct {
	t            *testing.T
	root, mirror string
}

func newCheckoutRepo(t *testing.T, files map[string]string) checkoutRepo {
	t.Helper()
	r := checkoutRepo{t: t, root: t.TempDir()}
	r.git("init", "-q")
	for rel, content := range files {
		r.write(rel, content)
	}
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", "base")
	r.mirror = filepath.Join(t.TempDir(), "mirror.git")
	r.git("clone", "-q", "--bare", r.root, r.mirror)
	return r
}

func (r checkoutRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.root, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "protocol.file.allow=always"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r checkoutRepo) write(rel, content string) {
	r.t.Helper()
	path := filepath.Join(r.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// host is a Host that only knows r as every run's checkout, enough for the file checks.
func (r checkoutRepo) host(base string) *Host {
	return &Host{runCheckout: func(context.Context, string, string) (deliverableCheckout, error) {
		return deliverableCheckout{WorkspaceLocation: store.WorkspaceLocation{Path: r.root, BaseCommit: base}, Mirror: r.mirror}, nil
	}}
}

func declarations(t *testing.T, specs ...string) []workflow.CompletionFile {
	t.Helper()
	var files []workflow.CompletionFile
	for _, spec := range specs {
		path, mustChange := strings.CutSuffix(spec, "!")
		f, err := workflow.NewCompletionFile(path, mustChange)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

func lower[T int | int64](t *testing.T, bound *T, to T) {
	t.Helper()
	was := *bound
	*bound = to
	t.Cleanup(func() { *bound = was })
}

// The checkout's git configuration is the Agent's: comparing with the base commit must not run a
// clean filter its .gitattributes names, nor an fsmonitor or hook, as v2-server.
func TestTheBaseCommitComparisonRunsNoProgramOfTheCheckout(t *testing.T) {
	r := newCheckoutRepo(t, map[string]string{"out/a.md": "before"})
	base := r.git("rev-parse", "HEAD")
	marker := filepath.Join(t.TempDir(), "ran")
	r.write(".gitattributes", "*.md filter=evil\n")
	r.git("config", "filter.evil.clean", "touch "+marker+"; cat")
	r.git("config", "core.fsmonitor", "touch "+marker)
	h := r.host(base)
	files := declarations(t, "out/*.md!")
	baseline := &fileBaseline{Source: baselineCommit}

	blockers, warnings := h.fileBlockers(t.Context(), "", "", files, baseline)
	if !reflect.DeepEqual(blockers, []completionBlocker{fileBlocker(blockerFileUnchanged, "out/*.md")}) || len(warnings) != 0 {
		t.Fatalf("unchanged since the commit: %v %v", blockers, warnings)
	}
	r.write("out/a.md", "after")
	if blockers, warnings := h.fileBlockers(t.Context(), "", "", files, baseline); len(blockers) != 0 || len(warnings) != 0 {
		t.Fatalf("changed since the commit: %v %v", blockers, warnings)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a program configured in the checkout ran: %v", err)
	}
}

// A wildcard lists only under its literal prefix and skips what git ignores, so a large ignored
// tree neither hides a deliverable nor counts as one; a listing the bound cuts short reports the
// check as unavailable instead of the file as missing.
func TestWildcardsListOnlyUnderTheirPrefixAndOutsideIgnoredTrees(t *testing.T) {
	r := newCheckoutRepo(t, map[string]string{".gitignore": "node_modules/\n"})
	for i := range 20 {
		r.write(fmt.Sprintf("node_modules/pkg/%d.md", i), "dependency")
	}
	r.write("reports/plan.md", "计划")
	h := r.host("")

	if blockers, warnings := h.fileBlockers(t.Context(), "", "", declarations(t, "reports/*.md"), nil); len(blockers) != 0 || len(warnings) != 0 {
		t.Fatalf("reports/*.md: %v %v", blockers, warnings)
	}
	l := listFiles(t.Context(), deliverableCheckout{WorkspaceLocation: store.WorkspaceLocation{Path: r.root}}, declarations(t, "**/*.md"))
	if !l.complete || !reflect.DeepEqual(l.found, map[string]bool{"reports/plan.md": true}) {
		t.Fatalf("**/*.md listed %v (complete %v), want only reports/plan.md", l.found, l.complete)
	}
	// A literal declaration is looked up directly, ignored or not.
	if blockers, _ := h.fileBlockers(t.Context(), "", "", declarations(t, "node_modules/pkg/1.md"), nil); len(blockers) != 0 {
		t.Fatalf("an ignored literal deliverable: %v", blockers)
	}

	lower(t, &maxListedEntries, 3)
	for i := range 5 {
		r.write(fmt.Sprintf("reports/extra-%d.txt", i), "x")
	}
	blockers, warnings := h.fileBlockers(t.Context(), "", "", declarations(t, "reports/*.csv", "reports/plan.md"), nil)
	if len(blockers) != 0 || !reflect.DeepEqual(warnings, []string{warningFileCheck}) {
		t.Fatalf("a truncated listing: %v %v, want file_check_unavailable and no blocker", blockers, warnings)
	}
}

// The checkout baseline is one digest per mustChange declaration, so it stays small however
// many files match; a declaration past the comparison bound is unavailable on both sides rather
// than judged on its first matches.
func TestTheCheckoutBaselineIsBoundedPerDeclaration(t *testing.T) {
	files := map[string]string{}
	for i := range 3 {
		files[fmt.Sprintf("out/%d.md", i)] = "before"
	}
	r := newCheckoutRepo(t, files)
	h := r.host("")
	decl := declarations(t, "out/*.md!")
	lower(t, &maxComparedFiles, 3)

	baseline := h.captureBaseline(t.Context(), "", "", decl)
	encoded, _ := json.Marshal(baseline)
	if baseline.Source != baselineCheckout || len(baseline.Declarations) != 1 || baseline.Declarations["out/*.md"] == "" || len(encoded) > 200 {
		t.Fatalf("baseline = %s", encoded)
	}
	if blockers, _ := h.fileBlockers(t.Context(), "", "", decl, baseline); !reflect.DeepEqual(blockers, []completionBlocker{fileBlocker(blockerFileUnchanged, "out/*.md")}) {
		t.Fatalf("unchanged: %v", blockers)
	}
	r.write("out/2.md", "after") // the last match in path order
	if blockers, warnings := h.fileBlockers(t.Context(), "", "", decl, baseline); len(blockers) != 0 || len(warnings) != 0 {
		t.Fatalf("the last match changed: %v %v", blockers, warnings)
	}

	r.write("out/3.md", "one too many")
	if blockers, warnings := h.fileBlockers(t.Context(), "", "", decl, baseline); len(blockers) != 0 || !reflect.DeepEqual(warnings, []string{warningFileCheck}) {
		t.Fatalf("past the bound: %v %v, want file_check_unavailable", blockers, warnings)
	}
	if over := h.captureBaseline(t.Context(), "", "", decl); over.Declarations["out/*.md"] != "" {
		t.Fatalf("a baseline past the bound = %v, want it unrecorded", over.Declarations)
	}
}

// The baseline is written once, as the node is dispatched, and a restarted v2-server still judges
// mustChange against it.
func TestTheFileBaselineSurvivesARestart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := newCheckoutRepo(t, map[string]string{"plan.md": "计划", "out/a.md": "before"})
	checkout := func(context.Context, string, string) (deliverableCheckout, error) {
		return deliverableCheckout{WorkspaceLocation: store.WorkspaceLocation{Path: r.root}}, nil
	}
	f.host.runCheckout = checkout
	runID, a := f.startParked(t, interactiveDeliverables, "初稿一")
	_, nodes := f.await(t, runID, parked("a"))
	stored := payloadOf(t, node(nodes, "a"))["completion_baseline"]
	f.exec.recordedTurns(a, node(nodes, "a").ID, completed("初稿一"))

	f.restart(t)
	f.host.runCheckout = checkout
	_, nodes = f.await(t, runID, awaitingInput("a"))
	if after := payloadOf(t, node(nodes, "a"))["completion_baseline"]; !reflect.DeepEqual(after, stored) {
		t.Fatalf("baseline after the restart = %v, want %v", after, stored)
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", confirm); !refusedWith(err, blockerFileUnchanged) {
		t.Fatalf("completion after the restart = %v, want out/*.md unchanged", err)
	}
	r.write("out/a.md", "after")
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", confirm); err != nil {
		t.Fatalf("completion after the change = %v", err)
	}
}

// Force covers blockers, never a park the starter did not see: a completion whose turn or answer
// changed is refused with force too, and leaves the node and its session as they were.
func TestForceCannotBypassTheStaleCheck(t *testing.T) {
	for name, tamper := range map[string]func(map[string]any){
		"turn":   func(c map[string]any) { c["turnKey"] = map[string]any{"userSeq": 7, "endedSeq": 9} },
		"output": func(c map[string]any) { c["outputDigest"] = "sha256:other" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			runID, a := f.startParked(t, interactiveChain, "初稿一")
			if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "继续"); err != nil {
				t.Fatal(err)
			}
			f.exec.awaitAppend(t)
			f.exec.finish(a, completed("定稿"))
			_, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
				return parked("a")(store.WorkflowRun{}, n) && interactivePayload(t, node(n, "a"))["turns"] == float64(2)
			})
			row := node(nodes, "a")
			summary := interactivePayload(t, row)
			tamper(summary["completion"].(map[string]any))
			if _, err := f.s.MoveWorkflowEngineNode(ctx, runID, row.ID, store.WorkflowStatusPending, store.WorkflowStatusPending,
				func(node []byte) ([]byte, error) {
					return workflow.MergePayload(node, map[string]any{"interactive": summary})
				}); err != nil {
				t.Fatal(err)
			}
			_, err := f.host.CompleteNode(ctx, f.scope, runID, "a", server.WorkflowNodeCompletion{Force: true})
			var turn *server.WorkflowNodeTurnChanged
			var output *server.WorkflowNodeOutputChanged
			if (name == "turn" && !errors.As(err, &turn)) || (name == "output" && !errors.As(err, &output)) {
				t.Fatalf("forced completion = %v, want the %s change", err, name)
			}
			if f.exec.isClosed(a) {
				t.Fatal("a stale forced completion ended the session")
			}
			_, nodes = f.await(t, runID, parked("a"))
			if p := payloadOf(t, node(nodes, "a")); node(nodes, "a").Status != store.WorkflowStatusPending || p["completion_forced"] != nil {
				t.Fatalf("a stale forced completion moved the node: %s %v", node(nodes, "a").Status, p["completion_forced"])
			}
		})
	}
}

// An Agent can make its checkout a partial clone whose promisor remote runs a program on a lazy
// fetch. The checks read committed content from the mirror and list through a scratch
// repository, so nothing configured in the checkout runs and nothing waits on a transport.
func TestAPromisorRemoteInTheCheckoutRunsNothing(t *testing.T) {
	r := newCheckoutRepo(t, map[string]string{"out/a.md": "before", "out/b.md": "kept"})
	base := r.git("rev-parse", "HEAD")
	marker := filepath.Join(t.TempDir(), "ran")
	prog := filepath.Join(t.TempDir(), "transport")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\ntouch "+marker+"\nsleep 600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.git("config", "core.repositoryformatversion", "1")
	r.git("config", "extensions.partialClone", "origin")
	r.git("config", "remote.origin.promisor", "true")
	r.git("config", "remote.origin.url", "ssh://evil.invalid/x")
	r.git("config", "core.sshCommand", prog)
	r.git("config", "protocol.ext.allow", "always")
	// The checkout no longer has out/a.md's blob: only a lazy fetch could produce it.
	blob := r.git("rev-parse", base+":out/a.md")
	if err := os.Remove(filepath.Join(r.root, ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	h := r.host(base)
	files := declarations(t, "out/*.md!", "out/a.md")

	started := time.Now()
	blockers, warnings := h.fileBlockers(t.Context(), "", "", files, &fileBaseline{Source: baselineCommit})
	if !reflect.DeepEqual(blockers, []completionBlocker{fileBlocker(blockerFileUnchanged, "out/*.md")}) || len(warnings) != 0 {
		t.Fatalf("checks = %v %v, want out/*.md unchanged since the mirror's base", blockers, warnings)
	}
	if baseline := h.captureBaseline(t.Context(), "", "", files); baseline.Declarations["out/*.md"] == "" {
		t.Fatalf("baseline = %v", baseline)
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("the checks took %s", elapsed)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the checkout's transport ran: %v", err)
	}
}

// The checkout baseline digests paths with contents: a match renamed with its content unchanged
// is a change.
func TestARenamedMatchIsAChange(t *testing.T) {
	r := newCheckoutRepo(t, map[string]string{"out/a.md": "same"})
	h := r.host("")
	decl := declarations(t, "out/*.md!")
	baseline := h.captureBaseline(t.Context(), "", "", decl)
	if err := os.Rename(filepath.Join(r.root, "out/a.md"), filepath.Join(r.root, "out/b.md")); err != nil {
		t.Fatal(err)
	}
	if blockers, warnings := h.fileBlockers(t.Context(), "", "", decl, baseline); len(blockers) != 0 || len(warnings) != 0 {
		t.Fatalf("a renamed match: %v %v, want a change", blockers, warnings)
	}
}

// The base commit's blob is compared as stored: no attribute rule, even one in the mirror that a
// filtered read would apply (here an eol conversion), turns an untouched file into a change.
func TestTheBaseCommitBlobIsComparedAsStored(t *testing.T) {
	r := newCheckoutRepo(t, map[string]string{"out/a.md": "line\n"})
	base := r.git("rev-parse", "HEAD")
	if err := os.MkdirAll(filepath.Join(r.mirror, "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.mirror, "info", "attributes"), []byte("*.md text eol=crlf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.write("out/a.md", "line\n") // as the Agent left it: untouched content
	h := r.host(base)
	blockers, _ := h.fileBlockers(t.Context(), "", "", declarations(t, "out/*.md!"), &fileBaseline{Source: baselineCommit})
	if !reflect.DeepEqual(blockers, []completionBlocker{fileBlocker(blockerFileUnchanged, "out/*.md")}) {
		t.Fatalf("an untouched file = %v, want file_unchanged", blockers)
	}
}

// A file the digest bound leaves unread is never judged unchanged against the base commit: a new
// or changed deliverable past the bound, at an exact path or matched by a glob, is
// file_check_unavailable.
func TestAFileTooLargeToDigestIsNotJudgedUnchanged(t *testing.T) {
	r := newCheckoutRepo(t, map[string]string{"out/a.md": "small"})
	base := r.git("rev-parse", "HEAD")
	h := r.host(base)
	lower(t, &maxDigestBytes, 8)
	r.write("dist/app.bin", "a new file past the bound")
	r.write("out/a.md", "changed past the bound")
	for _, decl := range []string{"dist/app.bin!", "out/*.md!"} {
		blockers, warnings := h.fileBlockers(t.Context(), "", "", declarations(t, decl), &fileBaseline{Source: baselineCommit})
		if len(blockers) != 0 || !reflect.DeepEqual(warnings, []string{warningFileCheck}) {
			t.Fatalf("%s: %v %v, want file_check_unavailable and no blocker", decl, blockers, warnings)
		}
	}
}
