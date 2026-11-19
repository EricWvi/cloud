package workspacereclaim_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/chat"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/execution/executiontest"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	st "github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
	"github.com/wanglongan587/cloud/v2-server/internal/workspacefs"
	wr "github.com/wanglongan587/cloud/v2-server/internal/workspacereclaim"
)

// diskRemover removes checkouts under root as the Agent runtime does, counting calls; fail makes
// the next call fail with an I/O-like error, and block holds every call until released.
type diskRemover struct {
	root  string
	calls atomic.Int32
	fail  atomic.Bool
	block chan struct{}
	enter chan struct{}
}

func (d *diskRemover) RemoveCheckout(ctx context.Context, _, _, path string) (workspacefs.Usage, error) {
	d.calls.Add(1)
	if d.enter != nil {
		d.enter <- struct{}{}
	}
	if d.block != nil {
		<-d.block
	}
	if d.fail.CompareAndSwap(true, false) {
		return workspacefs.Usage{}, errors.New("disk unavailable")
	}
	return workspacefs.RemoveCheckout(ctx, d.root, path)
}

type fixture struct {
	s       *store.Store
	scope   execution.Scope
	fs      *workspacefs.DeliveryFS
	remover *diskRemover
	r       *wr.Reclaimer
}

func newFixture(t *testing.T) *fixture {
	s := st.Open(t)
	user, tenant := st.Login(t, s, "reclaim-owner")
	fs, err := workspacefs.NewDeliveryFS(t.TempDir(), workspacefs.DefaultDeliveryObjectBytes)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{s: s, scope: execution.Scope{TenantID: tenant, UserID: user}, fs: fs, remover: &diskRemover{root: filepath.Join(t.TempDir(), "w", "r")}}
	f.r = wr.New(s, fs, f.remover)
	return f
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Agent", "-c", "user.email=agent@example.test", "-c", "init.defaultBranch=main"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, root, name, content string) {
	t.Helper()
	full := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// chat creates a conversation on a real clone with a mirror: every delivered turn first changes
// a file and is delivered for real, so the recorded commit holds the checkout of that moment.
func (f *fixture) chat(t *testing.T, outcomes ...st.TurnOutcome) st.ChatCheckout {
	t.Helper()
	path := filepath.Join(f.remover.root, st.UUID(t))
	write(t, path, "README.md", "base\n")
	write(t, path, ".gitignore", "*.log\n")
	git(t, path, "init", "-q", ".")
	git(t, path, "add", ".")
	git(t, path, "commit", "-qm", "base")
	base := git(t, path, "rev-parse", "HEAD")
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	git(t, filepath.Dir(mirror), "clone", "-q", "--mirror", path, mirror)
	project := st.UUID(t)
	repo := st.Repository{Mirror: mirror, Base: base, Deliver: func(i int) string {
		write(t, path, "turn.txt", strings.Repeat("x", i+1)+"\n")
		unlock, err := f.fs.LockWorkspace(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		snap, err := f.fs.Deliver(t.Context(), workspacefs.DeliveryRequest{Workspace: workspacefs.Workspace{Root: path, BaseCommit: base},
			MirrorPath: mirror, ProjectID: project, DeliveryID: st.UUID(t), Message: "test delivery"})
		if err != nil || snap.Commit == "" {
			t.Fatalf("deliver = %+v, %v", snap, err)
		}
		return snap.Commit
	}}
	return st.ChatWithRepository(t, f.s, f.scope, repo, path, outcomes...)
}

// deleteChat deletes the conversation as chat.Service does: end the session, then delete and
// record the reclaim.
func (f *fixture) deleteChat(t *testing.T, c st.ChatCheckout, mode store.ReclaimMode) {
	t.Helper()
	c.End(t, f.s)
	if _, err := f.s.DeleteChatConversationReclaiming(t.Context(), f.scope.TenantID, f.scope.UserID, c.ConversationID, mode); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) reclaim(t *testing.T, c st.ChatCheckout) store.WorkspaceReclaim {
	t.Helper()
	r, err := f.s.WorkspaceReclaim(t.Context(), c.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *fixture) run(t *testing.T, c st.ChatCheckout) wr.Outcome {
	t.Helper()
	out, err := f.r.Reclaim(t.Context(), c.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func TestReclaimRemovesOnlyRecoverableCheckoutsUnlessDiscarded(t *testing.T) {
	f := newFixture(t)
	recoverable := f.chat(t, st.TurnFailed, st.TurnDelivered)
	failed := f.chat(t, st.TurnDelivered, st.TurnFailed)
	legacy := f.chat(t, st.TurnUnenrolled)
	discarded := f.chat(t, st.TurnDelivered, st.TurnFailed)
	untouched := f.chat(t, st.TurnDelivered) // another conversation, not deleted
	f.deleteChat(t, recoverable, store.ReclaimRecoverable)
	f.deleteChat(t, failed, store.ReclaimRecoverable)
	f.deleteChat(t, legacy, store.ReclaimRecoverable)
	f.deleteChat(t, discarded, store.ReclaimDiscard)
	for _, tc := range []struct {
		c       st.ChatCheckout
		outcome wr.Outcome
	}{
		{recoverable, wr.Outcome{Workspace: wr.OutcomeReclaimed}},
		{failed, wr.Outcome{Workspace: wr.OutcomeKept, Reason: wr.ReasonLastUndelivered}},
		{legacy, wr.Outcome{Workspace: wr.OutcomeKept, Reason: wr.ReasonLegacy}},
		{discarded, wr.Outcome{Workspace: wr.OutcomeReclaimed}},
	} {
		got := f.run(t, tc.c)
		got.Bytes, got.Files = 0, 0
		if got != tc.outcome {
			t.Errorf("%s: outcome %+v, want %+v", tc.c.Path, got, tc.outcome)
		}
		if removed := !exists(t, tc.c.Path); removed != (tc.outcome.Workspace == wr.OutcomeReclaimed) {
			t.Errorf("%s: removed = %v", tc.c.Path, removed)
		}
	}
	if !exists(t, filepath.Join(untouched.Path, "README.md")) {
		t.Fatal("a conversation that was not deleted lost its checkout")
	}
	// The removal records what it released and the delivery it relied on.
	got := f.reclaim(t, recoverable)
	if got.Status != store.ReclaimReclaimed || got.RemovedBytes == 0 || got.RemovedFiles == 0 || got.DeliveryID == "" ||
		got.DeliveryCommit == "" || got.BaseCommit != recoverable.Repository.Base {
		t.Fatalf("reclaimed record = %+v", got)
	}
	// An operator restores the work from the mirror by those commits.
	if diff := git(t, recoverable.Repository.Mirror, "diff", "--stat", got.BaseCommit, got.DeliveryCommit); !strings.Contains(diff, "turn.txt") {
		t.Fatalf("the recorded delivery does not hold the work: %s", diff)
	}
}

func TestReclaimKeepsCheckoutsWithWorkNoDeliveryHolds(t *testing.T) {
	cases := map[string]struct {
		after func(t *testing.T, path string)
		want  string
	}{
		"written after the delivery": {func(t *testing.T, path string) { write(t, path, "late.txt", "late\n") }, wr.ReasonChanged},
		"stash": {func(t *testing.T, path string) {
			write(t, path, "README.md", "stashed\n")
			git(t, path, "stash", "push", "-q")
		}, wr.ReasonGitState},
		"branch beyond HEAD": {func(t *testing.T, path string) {
			git(t, path, "stash", "push", "-q", "-u")
			git(t, path, "checkout", "-q", "-b", "side")
			write(t, path, "side.txt", "side\n")
			git(t, path, "add", "side.txt")
			git(t, path, "commit", "-qm", "side")
			git(t, path, "checkout", "-q", "main")
			git(t, path, "stash", "pop", "-q")
		}, wr.ReasonGitState},
		"nested repository": {func(t *testing.T, path string) { write(t, path, "vendor/.git", "gitdir: /elsewhere\n") }, wr.ReasonGitState},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			c := f.chat(t, st.TurnDelivered)
			tc.after(t, c.Path)
			f.deleteChat(t, c, store.ReclaimRecoverable)
			if got := f.run(t, c); got.Workspace != wr.OutcomeKept || got.Reason != tc.want || !exists(t, c.Path) {
				t.Fatalf("outcome %+v, checkout exists %v", got, exists(t, c.Path))
			}
		})
	}
}

func TestReclaimWaitsForDeliveryWorkThenRemovesAtStartup(t *testing.T) {
	f := newFixture(t)
	c := f.chat(t, st.TurnDelivered, st.TurnDelivering)
	// Deleted while the last turn still delivers: recorded, but both modes wait.
	if _, err := f.s.DeleteChatConversationReclaiming(t.Context(), f.scope.TenantID, f.scope.UserID, c.ConversationID, store.ReclaimDiscard); err != nil {
		t.Fatal(err)
	}
	if got := f.run(t, c); got != (wr.Outcome{Workspace: wr.OutcomePending, Reason: wr.ReasonDelivering}) || !exists(t, c.Path) || f.remover.calls.Load() != 0 {
		t.Fatalf("reclaim ran before the delivery settled: %+v", got)
	}
	if got := f.reclaim(t, c); got.Status != store.ReclaimPending || got.Attempts != 0 || got.LastAttemptAt == nil {
		t.Fatalf("waiting reclaim = %+v", got)
	}
	c.Deliver(t, f.s, 1, st.TurnDelivered)
	c.End(t, f.s)
	if err := f.r.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.reclaim(t, c); got.Status != store.ReclaimReclaimed || exists(t, c.Path) {
		t.Fatalf("after startup = %+v, checkout exists %v", got, exists(t, c.Path))
	}
}

func TestDeletingReclaimIsFinishedWithoutJudgingAgain(t *testing.T) {
	f := newFixture(t)
	c := f.chat(t, st.TurnDelivered)
	f.deleteChat(t, c, store.ReclaimRecoverable)
	// Judged removable, then the process died mid-removal: part of the checkout is gone, and what
	// is left no longer matches the delivery.
	if moved, err := f.s.MarkWorkspaceDeleting(t.Context(), c.WorkspaceID, store.ReclaimBasis{BaseCommit: c.Repository.Base}); err != nil || !moved {
		t.Fatalf("mark deleting = %v, %v", moved, err)
	}
	if err := os.Remove(filepath.Join(c.Path, "turn.txt")); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.reclaim(t, c); got.Status != store.ReclaimReclaimed || exists(t, c.Path) {
		t.Fatalf("interrupted removal = %+v, checkout exists %v", got, exists(t, c.Path))
	}
}

func TestReclaimKeepsSharedLiveUnsafeAndForeignCheckouts(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	live := f.chat(t, st.TurnDelivered)
	if _, err := f.s.DeleteChatConversationReclaiming(ctx, f.scope.TenantID, f.scope.UserID, live.ConversationID, store.ReclaimDiscard); err != nil {
		t.Fatal(err)
	}
	shared := f.chat(t, st.TurnDelivered)
	turn := execution.Turn{Ref: execution.Ref{SessionID: "sibling-" + st.UUID(t), TurnID: "t-" + st.UUID(t)}, Prompt: execution.TextPrompt("x"),
		Caller: execution.Caller{Kind: execution.CallerChat, ID: "sibling", AttemptID: st.UUID(t)}}
	if err := f.s.CreateExecutionSession(ctx, f.scope, execution.Session{ID: turn.Ref.SessionID, WorkspaceID: shared.WorkspaceID,
		Target: &execution.Target{ProjectID: shared.ProjectID}, Model: execution.Model{ID: "fixture", Name: "Fixture", BindingVersion: "v1"}}, turn); err != nil {
		t.Fatal(err)
	}
	f.deleteChat(t, shared, store.ReclaimDiscard)
	// A checkout recorded outside the project's checkout root is never removed.
	outside := t.TempDir()
	unsafe := st.ChatWithCheckout(t, f.s, f.scope, outside, st.TurnUnenrolled)
	f.deleteChat(t, unsafe, store.ReclaimDiscard)
	// v2_workspaces now records another clone at the path: it is no longer this conversation's.
	foreign := f.chat(t, st.TurnDelivered)
	f.deleteChat(t, foreign, store.ReclaimDiscard)
	if err := f.s.RecordWorkspace(ctx, foreign.WorkspaceID, store.WorkspaceLocation{Path: foreign.Path, BaseCommit: foreign.Repository.Base, CloneExecution: "someone-else"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		c      st.ChatCheckout
		reason string
	}{{live, wr.ReasonLive}, {shared, wr.ReasonShared}, {unsafe, wr.ReasonUnsafe}, {foreign, wr.ReasonIdentityMismatch}} {
		if got := f.run(t, tc.c); got.Workspace != wr.OutcomeKept || got.Reason != tc.reason || !exists(t, tc.c.Path) {
			t.Errorf("%s: outcome %+v, checkout exists %v", tc.reason, got, exists(t, tc.c.Path))
		}
	}
}

func TestReclaimRetriesFailedRemovalAtStartup(t *testing.T) {
	f := newFixture(t)
	crashed := f.chat(t, st.TurnDelivered)
	failing := f.chat(t, st.TurnDelivered)
	// The deletions commit; the process dies before executing the first reclaim.
	f.deleteChat(t, crashed, store.ReclaimRecoverable)
	f.deleteChat(t, failing, store.ReclaimRecoverable)
	f.remover.fail.Store(true)
	if got := f.run(t, failing); got != (wr.Outcome{Workspace: wr.OutcomePending, Reason: wr.ReasonRemovalFailed}) {
		t.Fatalf("failed removal = %+v", got)
	}
	if got := f.reclaim(t, failing); got.Status != store.ReclaimDeleting || got.Attempts != 1 || got.LastError == "" || !exists(t, failing.Path) {
		t.Fatalf("after failure: %+v", got)
	}
	restarted := wr.New(f.s, f.fs, f.remover)
	if err := restarted.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []st.ChatCheckout{crashed, failing} {
		if got := f.reclaim(t, c); got.Status != store.ReclaimReclaimed || exists(t, c.Path) {
			t.Fatalf("after restart: %+v, checkout exists %v", got, exists(t, c.Path))
		}
	}
}

func TestConcurrentReclaimsRemoveOnce(t *testing.T) {
	f := newFixture(t)
	c := f.chat(t, st.TurnDelivered)
	f.deleteChat(t, c, store.ReclaimRecoverable)
	f.remover.block, f.remover.enter = make(chan struct{}), make(chan struct{}, 4)
	var wg sync.WaitGroup
	outs := make(chan wr.Outcome, 3)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := f.r.Reclaim(t.Context(), c.WorkspaceID)
			if err != nil {
				t.Error(err)
			}
			outs <- out
		}()
	}
	<-f.remover.enter // one execution is removing; the others wait for it
	close(f.remover.block)
	wg.Wait()
	close(outs)
	for out := range outs {
		if out.Workspace != wr.OutcomeReclaimed {
			t.Fatalf("outcome %+v", out)
		}
	}
	if n := f.remover.calls.Load(); n != 1 {
		t.Fatalf("checkout removed %d times", n)
	}
}

func TestPreviewTellsWhatDeletingDoes(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	preview := func(id string) wr.Verdict {
		t.Helper()
		v, err := f.r.Preview(ctx, f.scope.TenantID, f.scope.UserID, id)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	c := f.chat(t, st.TurnDelivered)
	v := preview(c.ConversationID)
	if v.Workspace != wr.WorkspaceRecoverable || v.Ignored || v.LatestDelivery == "" || v.Revision == "" || len(v.Changes) != 0 {
		t.Fatalf("recoverable preview = %+v", v)
	}
	write(t, c.Path, "build.log", "ignored\n")
	if v := preview(c.ConversationID); v.Workspace != wr.WorkspaceRecoverable || !v.Ignored {
		t.Fatalf("ignored files not reported: %+v", v)
	}
	// After the delivery a file changed: listed against that delivery, and the revision moves.
	write(t, c.Path, "late.txt", "late\n")
	late := preview(c.ConversationID)
	if late.Workspace != wr.WorkspaceUndelivered || late.Reason != wr.ReasonChanged || late.Total != 1 ||
		late.Changes[0].Path != "late.txt" || late.Revision == v.Revision {
		t.Fatalf("changed preview = %+v", late)
	}
	if again := preview(c.ConversationID); again.Revision != late.Revision {
		t.Fatal("the same state gave another revision")
	}
	// A conversation whose checkout is gone, and one never given a checkout.
	gone := st.ChatWithCheckout(t, f.s, f.scope, filepath.Join(f.remover.root, "gone"), st.TurnUnenrolled)
	if v := preview(gone.ConversationID); v.Workspace != wr.WorkspaceNone || v.Reason != wr.ReasonMissing {
		t.Fatalf("missing checkout preview = %+v", v)
	}
	bare, err := f.s.CreateChatConversation(ctx, store.NewChatConversation{TenantID: f.scope.TenantID, UserID: f.scope.UserID,
		CreateKey: "bare", ProjectID: "p", ExecutorID: "fixture", Title: "bare"})
	if err != nil {
		t.Fatal(err)
	}
	if v := preview(bare.ID); v.Workspace != wr.WorkspaceNone || v.Reason != wr.ReasonNoWorkspace {
		t.Fatalf("no checkout preview = %+v", v)
	}
}

func TestOperatorDiscardsAKeptCheckout(t *testing.T) {
	f := newFixture(t)
	c := f.chat(t, st.TurnUnenrolled)
	f.deleteChat(t, c, store.ReclaimRecoverable)
	if got := f.run(t, c); got.Workspace != wr.OutcomeKept {
		t.Fatalf("legacy checkout = %+v", got)
	}
	if kept, err := f.s.WorkspaceReclaims(t.Context(), store.ReclaimKept); err != nil || len(kept) != 1 || kept[0].Path != c.Path {
		t.Fatalf("kept listing = %+v, %v", kept, err)
	}
	out, err := f.r.DiscardKept(t.Context(), c.WorkspaceID)
	if err != nil || out.Workspace != wr.OutcomeReclaimed || exists(t, c.Path) {
		t.Fatalf("discard = %+v, %v", out, err)
	}
	if got := f.reclaim(t, c); got.Status != store.ReclaimReclaimed || got.Reason != wr.ReasonOperatorDiscard {
		t.Fatalf("record = %+v", got)
	}
	if _, err := f.r.DiscardKept(t.Context(), c.WorkspaceID); err == nil {
		t.Fatal("a reclaimed checkout was discarded again")
	}
}

// A discard whose preview showed other content than the checkout now holds is stale even when
// the listed path and line counts are the same, and nothing changes.
func TestDiscardOfRewrittenContentIsStale(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.chat(t, st.TurnDelivered)
	c.End(t, f.s)
	if err := f.s.EndChatSession(ctx, c.SessionID, store.EndStopped, time.Now()); err != nil {
		t.Fatal(err)
	}
	stub := &executiontest.Stub{}
	svc := chat.New(f.s, stub, stub, chat.Options{Reclaims: f.r})
	write(t, c.Path, "late.txt", "old\n")
	seen, err := svc.DeletionPreview(ctx, f.scope, c.ConversationID)
	if err != nil || seen.Reason != wr.ReasonChanged || seen.Total != 1 {
		t.Fatalf("preview = %+v, %v", seen, err)
	}
	write(t, c.Path, "late.txt", "new\n") // same path, same one added line
	now, err := svc.DeletionPreview(ctx, f.scope, c.ConversationID)
	if err != nil || now.Revision == seen.Revision {
		t.Fatalf("rewritten content kept the revision %q: %+v, %v", seen.Revision, now, err)
	}
	if _, err := svc.Delete(ctx, f.scope, c.ConversationID, store.ReclaimDiscard, seen.Revision); !errors.Is(err, chat.ErrDeletionPreviewStale) {
		t.Fatalf("discard with the old revision = %v", err)
	}
	if _, err := f.s.ChatConversation(ctx, f.scope.TenantID, f.scope.UserID, c.ConversationID); err != nil {
		t.Fatalf("the conversation went: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(c.Path, "late.txt")); err != nil || string(got) != "new\n" {
		t.Fatalf("the checkout changed: %q, %v", got, err)
	}
	if list, err := f.s.WorkspaceReclaims(ctx); err != nil || len(list) != 0 {
		t.Fatalf("a stale discard recorded a reclaim: %+v, %v", list, err)
	}
	if out, err := svc.Delete(ctx, f.scope, c.ConversationID, store.ReclaimDiscard, now.Revision); err != nil || out.Workspace != wr.OutcomeReclaimed {
		t.Fatalf("discard with the current revision = %+v, %v", out, err)
	}
}

// The mirror is unreachable at the first attempt: the reclaim stays pending with the error and a
// later start, with the mirror back, reclaims it.
func TestReclaimRetriesWhenTheCheckCannotRun(t *testing.T) {
	f := newFixture(t)
	c := f.chat(t, st.TurnDelivered)
	f.deleteChat(t, c, store.ReclaimRecoverable)
	away := c.Repository.Mirror + ".away"
	if err := os.Rename(c.Repository.Mirror, away); err != nil {
		t.Fatal(err)
	}
	if got := f.run(t, c); got != (wr.Outcome{Workspace: wr.OutcomePending, Reason: wr.ReasonCheckFailed}) || !exists(t, c.Path) {
		t.Fatalf("with the mirror away = %+v", got)
	}
	if got := f.reclaim(t, c); got.Status != store.ReclaimPending || got.Attempts != 1 || got.LastError == "" {
		t.Fatalf("after the failed check: %+v", got)
	}
	if err := os.Rename(away, c.Repository.Mirror); err != nil {
		t.Fatal(err)
	}
	if err := wr.New(f.s, f.fs, f.remover).Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.reclaim(t, c); got.Status != store.ReclaimReclaimed || exists(t, c.Path) {
		t.Fatalf("after the restart: %+v, checkout exists %v", got, exists(t, c.Path))
	}
}

// git_state previews say what the repository holds instead of an empty change list.
func TestGitStatePreviewNamesWhatTheRepositoryHolds(t *testing.T) {
	f := newFixture(t)
	c := f.chat(t, st.TurnDelivered)
	write(t, c.Path, "README.md", "stashed\n")
	git(t, c.Path, "stash", "push", "-q")
	write(t, c.Path, "data/.git", "gitdir: /elsewhere\n")
	v, err := f.r.Preview(t.Context(), f.scope.TenantID, f.scope.UserID, c.ConversationID)
	if err != nil || v.Reason != wr.ReasonGitState || v.Git.Stashes != 1 || len(v.Git.Repositories) != 1 || v.Git.Repositories[0] != "data" {
		t.Fatalf("preview = %+v, %v", v, err)
	}
	// Another stash changes the revision although the change list stays the same.
	write(t, c.Path, "README.md", "stashed again\n")
	git(t, c.Path, "stash", "push", "-q")
	if again, err := f.r.Preview(t.Context(), f.scope.TenantID, f.scope.UserID, c.ConversationID); err != nil || again.Git.Stashes != 2 || again.Revision == v.Revision {
		t.Fatalf("second stash = %+v, %v", again, err)
	}
}

// An initialized submodule with uncommitted changes, and a checkout whose .git points nowhere,
// keep the checkout: the first holds work no delivery has, the second cannot be verified.
func TestReclaimKeepsSubmodulesAndUnverifiableCheckouts(t *testing.T) {
	f := newFixture(t)
	sub := f.chat(t, st.TurnDelivered)
	lib := t.TempDir()
	write(t, lib, "lib.go", "package lib\n")
	git(t, lib, "init", "-q", ".")
	git(t, lib, "add", ".")
	git(t, lib, "commit", "-qm", "lib")
	git(t, sub.Path, "-c", "protocol.file.allow=always", "submodule", "add", "-q", lib, "mod")
	write(t, sub.Path, "mod/lib.go", "package lib // edited\n")
	f.deleteChat(t, sub, store.ReclaimRecoverable)
	if got := f.run(t, sub); got != (wr.Outcome{Workspace: wr.OutcomeKept, Reason: wr.ReasonGitState}) || !exists(t, filepath.Join(sub.Path, "mod", "lib.go")) {
		t.Fatalf("submodule outcome = %+v", got)
	}
	broken := f.chat(t, st.TurnDelivered)
	if err := os.RemoveAll(filepath.Join(broken.Path, ".git")); err != nil {
		t.Fatal(err)
	}
	write(t, broken.Path, ".git", "gitdir: /nowhere/at/all\n")
	f.deleteChat(t, broken, store.ReclaimRecoverable)
	if got := f.run(t, broken); got != (wr.Outcome{Workspace: wr.OutcomeKept, Reason: wr.ReasonUnverifiable}) || !exists(t, broken.Path) {
		t.Fatalf("broken .git outcome = %+v", got)
	}
}

// Closing the reclaimer stops a background removal; the reclaim stays deleting and the next start
// finishes it.
func TestBackgroundRemovalStoppedAtShutdownIsFinishedAtStart(t *testing.T) {
	f := newFixture(t)
	c := f.chat(t, st.TurnDelivered)
	f.deleteChat(t, c, store.ReclaimRecoverable)
	f.remover.block, f.remover.enter = make(chan struct{}), make(chan struct{}, 1)
	result := f.r.Start(c.WorkspaceID)
	<-f.remover.enter
	closed := make(chan struct{})
	go func() { f.r.Close(); close(closed) }()
	time.Sleep(20 * time.Millisecond) // Close has cancelled; the removal resumes and sees it
	close(f.remover.block)
	<-closed
	if res := <-result; res.Err != nil || res.Outcome.Workspace != wr.OutcomePending {
		t.Fatalf("stopped removal = %+v", res)
	}
	if got := f.reclaim(t, c); got.Status != store.ReclaimDeleting || !exists(t, c.Path) {
		t.Fatalf("after shutdown = %+v", got)
	}
	f.remover.block, f.remover.enter = nil, nil
	if err := wr.New(f.s, f.fs, f.remover).Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.reclaim(t, c); got.Status != store.ReclaimReclaimed || exists(t, c.Path) {
		t.Fatalf("after start = %+v", got)
	}
}

// discardAfter previews the conversation, lets change alter the repository, and checks that a
// discard with the earlier revision is stale and changes nothing.
func discardAfter(t *testing.T, f *fixture, c st.ChatCheckout, change func()) {
	t.Helper()
	ctx := context.Background()
	c.End(t, f.s)
	if err := f.s.EndChatSession(ctx, c.SessionID, store.EndStopped, time.Now()); err != nil {
		t.Fatal(err)
	}
	stub := &executiontest.Stub{}
	svc := chat.New(f.s, stub, stub, chat.Options{Reclaims: f.r})
	seen, err := svc.DeletionPreview(ctx, f.scope, c.ConversationID)
	if err != nil || seen.Reason != wr.ReasonGitState {
		t.Fatalf("preview = %+v, %v", seen, err)
	}
	change()
	now, err := svc.DeletionPreview(ctx, f.scope, c.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(now.Git, seen.Git) {
		t.Fatalf("the shown findings differ, so this case proves nothing: %+v vs %+v", seen.Git, now.Git)
	}
	if _, err := svc.Delete(ctx, f.scope, c.ConversationID, store.ReclaimDiscard, seen.Revision); !errors.Is(err, chat.ErrDeletionPreviewStale) {
		t.Fatalf("discard with the earlier revision = %v", err)
	}
	if _, err := f.s.ChatConversation(ctx, f.scope.TenantID, f.scope.UserID, c.ConversationID); err != nil {
		t.Fatalf("the conversation went: %v", err)
	}
	if !exists(t, filepath.Join(c.Path, ".git")) {
		t.Fatal("the checkout went")
	}
}

// Another stash in place of the one previewed: the same count, a different stash.
func TestDiscardIsStaleWhenTheStashIsReplaced(t *testing.T) {
	f := newFixture(t)
	c := f.chat(t, st.TurnDelivered)
	write(t, c.Path, "README.md", "first stash\n")
	git(t, c.Path, "stash", "push", "-q")
	discardAfter(t, f, c, func() {
		git(t, c.Path, "stash", "drop", "-q")
		write(t, c.Path, "README.md", "second stash\n")
		git(t, c.Path, "stash", "push", "-q")
	})
}

// A ref moved to other commits, as many beyond the base and HEAD as before.
func TestDiscardIsStaleWhenARefMovesWithTheSameCount(t *testing.T) {
	f := newFixture(t)
	c := f.chat(t, st.TurnDelivered)
	git(t, c.Path, "stash", "push", "-q", "-u")
	for _, n := range []string{"a", "b"} {
		git(t, c.Path, "checkout", "-q", "-b", "side-"+n, "main")
		write(t, c.Path, "side.txt", n+"\n")
		git(t, c.Path, "add", "side.txt")
		git(t, c.Path, "commit", "-qm", "side "+n)
	}
	git(t, c.Path, "checkout", "-q", "main")
	git(t, c.Path, "branch", "-D", "-q", "side-b")
	git(t, c.Path, "stash", "pop", "-q")
	// side-a reaches one commit beyond HEAD; afterwards it reaches a different one.
	var other string
	discardAfter(t, f, c, func() {
		git(t, c.Path, "stash", "push", "-q", "-u")
		git(t, c.Path, "checkout", "-q", "-b", "tmp", "main")
		write(t, c.Path, "side.txt", "c\n")
		git(t, c.Path, "add", "side.txt")
		git(t, c.Path, "commit", "-qm", "side c")
		other = git(t, c.Path, "rev-parse", "HEAD")
		git(t, c.Path, "checkout", "-q", "main")
		git(t, c.Path, "branch", "-q", "-f", "side-a", other)
		git(t, c.Path, "branch", "-D", "-q", "tmp")
		git(t, c.Path, "stash", "pop", "-q")
	})
}

// nestedRepository makes vendor/lib a repository of its own with one committed file.
func nestedRepository(t *testing.T, root, content string) string {
	t.Helper()
	sub := filepath.Join(root, "vendor", "lib")
	write(t, sub, "lib.txt", content)
	git(t, sub, "init", "-q", ".")
	git(t, sub, "add", ".")
	git(t, sub, "commit", "-qm", "lib")
	return sub
}

// A nested file rewritten to the same size with its old modification time is still a change the
// earlier preview did not show.
func TestDiscardIsStaleWhenANestedFileIsRewrittenInPlace(t *testing.T) {
	f := newFixture(t)
	c := f.chat(t, st.TurnDelivered)
	sub := nestedRepository(t, c.Path, "aaaa\n")
	file := filepath.Join(sub, "lib.txt")
	discardAfter(t, f, c, func() {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		write(t, sub, "lib.txt", "bbbb\n")
		if err := os.Chtimes(file, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
	})
	if got, err := os.ReadFile(file); err != nil || string(got) != "bbbb\n" {
		t.Fatalf("the nested file changed: %q, %v", got, err)
	}
}

// A nested repository too large to bind into the revision cannot be discarded: the preview says
// so and a discard with the current revision is refused, leaving everything.
func TestDiscardIsRefusedWhenANestedRepositoryIsTooLarge(t *testing.T) {
	s := st.Open(t)
	user, tenant := st.Login(t, s, "reclaim-owner")
	fs, err := workspacefs.NewDeliveryFS(t.TempDir(), 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{s: s, scope: execution.Scope{TenantID: tenant, UserID: user}, fs: fs, remover: &diskRemover{root: filepath.Join(t.TempDir(), "w", "r")}}
	f.r = wr.New(s, fs, f.remover)
	c := f.chat(t, st.TurnDelivered)
	nestedRepository(t, c.Path, strings.Repeat("x", 100<<10))
	c.End(t, f.s)
	ctx := context.Background()
	if err := f.s.EndChatSession(ctx, c.SessionID, store.EndStopped, time.Now()); err != nil {
		t.Fatal(err)
	}
	stub := &executiontest.Stub{}
	svc := chat.New(f.s, stub, stub, chat.Options{Reclaims: f.r})
	v, err := svc.DeletionPreview(ctx, f.scope, c.ConversationID)
	if err != nil || v.Reason != wr.ReasonGitState || v.Discardable {
		t.Fatalf("preview = %+v, %v", v, err)
	}
	if _, err := svc.Delete(ctx, f.scope, c.ConversationID, store.ReclaimDiscard, v.Revision); !errors.Is(err, chat.ErrDiscardUnverifiable) {
		t.Fatalf("discard = %v, want ErrDiscardUnverifiable", err)
	}
	if _, err := f.s.ChatConversation(ctx, f.scope.TenantID, f.scope.UserID, c.ConversationID); err != nil || !exists(t, filepath.Join(c.Path, "vendor", "lib", "lib.txt")) {
		t.Fatalf("a refused discard changed something: %v", err)
	}
}
