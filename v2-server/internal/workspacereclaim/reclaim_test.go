package workspacereclaim_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
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

func (d *diskRemover) RemoveCheckout(_, _, path string) (workspacefs.Usage, error) {
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
	return workspacefs.RemoveCheckout(d.root, path)
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
