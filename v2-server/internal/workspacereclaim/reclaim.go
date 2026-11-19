// Package workspacereclaim judges and removes the checkouts of conversations the person deleted:
// a checkout goes only when its work is recoverable from the session's deliveries, or when the
// person chose to discard it.
//
// Deleting a conversation records a reclaim in the same transaction (see
// store.DeleteChatConversationReclaiming). The Reclaimer executes it right after the deletion and,
// for every reclaim still open, at startup (boot.Recover, after interrupted deliveries are
// recovered and before workflows resume or requests are served). A reclaim moves pending →
// deleting → reclaimed | kept. A pending one is judged on every execution; once judged removable
// it is deleting, and from then on only the path's identity is checked again, so a removal
// interrupted halfway is finished rather than kept.
package workspacereclaim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workspacefs"
)

// What a preview or a judgement says about a checkout (Verdict.Workspace).
const (
	WorkspaceNone        = "none"
	WorkspaceRecoverable = "recoverable"
	WorkspaceUndelivered = "undelivered"
)

// What executing a reclaim came to (Outcome.Workspace), besides WorkspaceNone.
const (
	OutcomeReclaimed = "reclaimed"
	OutcomeKept      = "kept"
	OutcomePending   = "pending"
)

// Why a checkout is none or undelivered (Verdict.Reason), and why a reclaim ended kept.
const (
	ReasonNoWorkspace     = "no_workspace"      // none: the conversation has no checkout recorded
	ReasonMissing         = "workspace_missing" // none: the recorded checkout no longer exists
	ReasonLegacy          = "legacy"            // the session never recorded a delivery
	ReasonDelivering      = "delivering"        // a turn runs or its delivery has not settled
	ReasonLastUndelivered = "last_turn_undelivered"
	ReasonGitState        = "git_state"              // the repository holds work no delivery carries
	ReasonChanged         = "changed_after_delivery" // files changed after the last delivery
	ReasonUnverifiable    = "unverifiable"           // the snapshot tree could not be built

	ReasonShared             = "shared"              // kept: another session used the checkout
	ReasonLive               = "session_live"        // kept: a session on the checkout is still live
	ReasonUnsafe             = "unsafe_path"         // kept: outside the checkout root, a symlink, or swapped
	ReasonIdentityMismatch   = "identity_mismatch"   // kept: v2_workspaces no longer records this path
	ReasonRemovalFailed      = "removal_failed"      // pending: the removal failed and is retried at startup
	ReasonReclaimUnavailable = "reclaim_unavailable" // pending: this server has no Agent runtime to judge it
	ReasonCheckFailed        = "check_failed"        // pending: the checkout could not be inspected (disk, mirror, Git); retried
	ReasonInProgress         = "in_progress"         // pending: still being removed in the background
	ReasonOperatorDiscard    = "operator_discard"    // reclaimed: an operator discarded a kept checkout
)

// Verdict is what deleting a conversation does to its checkout, as of now.
type Verdict struct {
	Workspace, Reason string
	// Ignored reports, for a recoverable checkout, files the ignore rules leave out of deliveries.
	Ignored bool
	// Changes lists undelivered changes (against the newest good delivery, else the base), at
	// most workspacefs.MaxReclaimChanges; Total counts them all.
	Changes []workspacefs.Change
	Total   int
	// Git is what the repository holds that no delivery carries (for git_state).
	Git workspacefs.GitFindings
	// LatestDelivery is the newest delivered (not empty) delivery, for a patch download.
	LatestDelivery string
	// Revision identifies the state this verdict rests on; a discard must present it.
	Revision string
	// Discardable is false when the revision cannot vouch for everything a discard would drop
	// (a nested repository too large to digest): a discard is refused until it is dealt with.
	Discardable bool

	basis store.ReclaimBasis
}

// Outcome is what executing a reclaim came to: reclaimed (with what it released), kept or
// pending (with a reason), or none when there was no checkout.
type Outcome struct {
	Workspace, Reason string
	Bytes, Files      int64
}

// Store is the persistence the Reclaimer needs; *store.Store implements it.
type Store interface {
	ChatCheckout(ctx context.Context, tenant, user, conversationID string) (store.ChatCheckout, error)
	WorkspaceReclaim(ctx context.Context, workspaceID string) (store.WorkspaceReclaim, error)
	WorkspaceReclaims(ctx context.Context, statuses ...store.ReclaimStatus) ([]store.WorkspaceReclaim, error)
	ReclaimCheckout(ctx context.Context, r store.WorkspaceReclaim) (store.Checkout, error)
	CheckReclaimIdentity(ctx context.Context, r store.WorkspaceReclaim) (store.ReclaimIdentity, error)
	MarkWorkspaceDeleting(ctx context.Context, workspaceID string, basis store.ReclaimBasis) (bool, error)
	FinishWorkspaceReclaimed(ctx context.Context, workspaceID string, basis store.ReclaimBasis) error
	FinishWorkspaceKept(ctx context.Context, workspaceID, reason string) error
	NoteWorkspaceReclaimAttempt(ctx context.Context, workspaceID, lastError string) error
}

// Inspector builds a checkout's snapshot tree by the delivery rules and compares it, under the
// delivery's lock on the checkout; *workspacefs.DeliveryFS implements it.
type Inspector interface {
	LockWorkspace(ctx context.Context, root string) (func(), error)
	InspectReclaim(ctx context.Context, ws workspacefs.Workspace, mirrorPath, against string) (workspacefs.ReclaimInspection, error)
}

// Remover deletes a checkout after detaching whatever this process still holds for it. It
// returns workspacefs.ErrUnsafeCheckout for a path it may not delete and
// workspacefs.ErrCheckoutInUse for a checkout a live session in this process holds.
type Remover interface {
	RemoveCheckout(ctx context.Context, projectID, workspaceID, path string) (workspacefs.Usage, error)
}

// Reclaimer judges and executes workspace reclaims, one execution per checkout at a time.
type Reclaimer struct {
	store   Store
	inspect Inspector
	remover Remover

	// ctx is the server's life: Start runs under it, and Close cancels it and waits.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu    sync.Mutex
	locks map[string]*keyed
}

type keyed struct {
	sync.Mutex
	holders int
}

// New returns a Reclaimer. Close must be called before the store is closed.
func New(s Store, i Inspector, r Remover) *Reclaimer {
	ctx, cancel := context.WithCancel(context.Background())
	return &Reclaimer{store: s, inspect: i, remover: r, locks: map[string]*keyed{}, ctx: ctx, cancel: cancel}
}

// Result is what a reclaim Start began came to.
type Result struct {
	Outcome Outcome
	Err     error
}

// Start executes a reclaim in the background, under the server's life rather than a request's:
// the deletion has committed, and the removal must not stop because a client went away. The
// channel delivers the result once. Close cancels it; a removal stopped that way stays deleting
// and the next start finishes it.
func (r *Reclaimer) Start(workspaceID string) <-chan Result {
	done := make(chan Result, 1)
	r.mu.Lock()
	if r.ctx.Err() != nil {
		// Stopping: the reclaim stays recorded for the next start.
		r.mu.Unlock()
		done <- Result{Outcome: Outcome{Workspace: OutcomePending, Reason: ReasonInProgress}}
		return done
	}
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		out, err := r.Reclaim(r.ctx, workspaceID)
		done <- Result{out, err}
	}()
	return done
}

// Close cancels background executions and waits for them to stop.
func (r *Reclaimer) Close() {
	r.mu.Lock()
	r.cancel()
	r.mu.Unlock()
	r.wg.Wait()
}

// lock serializes executions of one reclaim within this process; the process holds the data
// directory lock, so no other process executes it.
func (r *Reclaimer) lock(id string) func() {
	r.mu.Lock()
	l := r.locks[id]
	if l == nil {
		l = &keyed{}
		r.locks[id] = l
	}
	l.holders++
	r.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		r.mu.Lock()
		if l.holders--; l.holders == 0 {
			delete(r.locks, id)
		}
		r.mu.Unlock()
	}
}

// Preview judges what deleting one of the user's conversations would do to its checkout.
// store.ErrNotFound means the conversation is not theirs.
func (r *Reclaimer) Preview(ctx context.Context, tenant, user, conversationID string) (Verdict, error) {
	c, err := r.store.ChatCheckout(ctx, tenant, user, conversationID)
	if err != nil {
		return Verdict{}, err
	}
	if !c.Registered {
		return seal(Verdict{Workspace: WorkspaceNone, Reason: ReasonNoWorkspace}, c.Checkout, ""), nil
	}
	v, unlock, err := r.judge(ctx, c.Checkout)
	if unlock != nil {
		unlock()
	}
	return v, err
}

// judge applies the recovery conditions in order and reports the first that fails. When it
// inspects the checkout it returns holding the delivery's lock on it; the caller releases it.
func (r *Reclaimer) judge(ctx context.Context, c store.Checkout) (Verdict, func(), error) {
	d := c.Delivery
	if _, err := os.Lstat(c.Path); errors.Is(err, fs.ErrNotExist) {
		return seal(Verdict{Workspace: WorkspaceNone, Reason: ReasonMissing}, c, ""), nil, nil
	} else if err != nil {
		return Verdict{}, nil, fmt.Errorf("inspect checkout: %w", err)
	}
	if d.Busy {
		return seal(Verdict{Workspace: WorkspaceUndelivered, Reason: ReasonDelivering}, c, ""), nil, nil
	}
	lastGood := d.LastStatus == store.DeliveryDelivered || d.LastStatus == store.DeliveryEmpty
	against := d.GoodCommit // undelivered changes are listed against the newest good delivery
	if lastGood {
		against = d.LastCommit // and compared with the last turn's; an empty one leaves the base
	}
	// Inspected also for a legacy session (against the base): the snapshot tree binds the
	// revision to what the checkout holds, which is what a discard drops.
	unlock, err := r.inspect.LockWorkspace(ctx, c.Path)
	if err != nil {
		return Verdict{}, nil, err
	}
	in, err := r.inspect.InspectReclaim(ctx, workspacefs.Workspace{Root: c.Path, BaseCommit: c.BaseCommit}, c.MirrorPath, against)
	switch {
	case errors.Is(err, workspacefs.ErrGone):
		unlock()
		return seal(Verdict{Workspace: WorkspaceNone, Reason: ReasonMissing}, c, ""), nil, nil
	case errors.Is(err, workspacefs.ErrUnverifiable):
		// What the checkout holds cannot be told, now or later.
		reason := ReasonUnverifiable
		if !d.Delivered {
			reason = ReasonLegacy
		} else if !lastGood {
			reason = ReasonLastUndelivered
		}
		return seal(Verdict{Workspace: WorkspaceUndelivered, Reason: reason}, c, ""), unlock, nil
	case err != nil:
		// The disk, the mirror or Git failed: nothing is concluded, a later attempt retries.
		unlock()
		return Verdict{}, nil, fmt.Errorf("inspect checkout: %w", err)
	}
	v := Verdict{Workspace: WorkspaceUndelivered, Changes: in.Changes, Total: in.Total, Git: in.Git, Discardable: in.GitIdentityComplete}
	switch {
	case !d.Delivered:
		v.Reason = ReasonLegacy
	case !lastGood:
		v.Reason = ReasonLastUndelivered
	case !in.Git.Empty():
		v.Reason = ReasonGitState
	case !in.Unchanged:
		v.Reason = ReasonChanged
	default:
		v = Verdict{Workspace: WorkspaceRecoverable, Ignored: in.Ignored,
			basis: store.ReclaimBasis{DeliveryID: d.LastDeliveryID, DeliveryCommit: d.LastCommit, BaseCommit: c.BaseCommit}}
	}
	git, err := json.Marshal(in.Git)
	if err != nil {
		unlock()
		return Verdict{}, nil, err
	}
	// The counts show what is held; the identity digest binds the actual stash entries, ref
	// targets and nested repositories, which the counts alone would not.
	return seal(v, c, in.Tree+"\x00"+string(git)+"\x00"+in.GitIdentity), unlock, nil
}

// seal fills in what every verdict carries: the newest good delivery and the revision, a digest
// of the last turn, the deliveries, the verdict and content: the checkout's snapshot tree and
// Git state when it was inspected, which change whenever a file the snapshot takes changes.
func seal(v Verdict, c store.Checkout, content string) Verdict {
	v.LatestDelivery = c.Delivery.GoodDeliveryID
	if v.Workspace != WorkspaceUndelivered || v.Reason == ReasonDelivering {
		v.Discardable = false // nothing to discard, or not yet
	} else if content == "" {
		v.Discardable = true // not inspected (unverifiable): what is shown is all there is to show
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%s\x00", c.WorkspaceID, c.Delivery.LastTurnID, c.Delivery.LastStatus,
		c.Delivery.GoodDeliveryID, c.Delivery.LastDeliveryID, v.Workspace, v.Reason, v.Total, content)
	for _, ch := range v.Changes {
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\x00", ch.Path, ch.Status, ch.Additions, ch.Deletions)
	}
	v.Revision = hex.EncodeToString(h.Sum(nil))[:32]
	return v
}

// Reclaim executes one reclaim and returns what it came to. A failed removal is left for the
// next start and reported as pending, not as an error.
func (r *Reclaimer) Reclaim(ctx context.Context, workspaceID string) (Outcome, error) {
	defer r.lock(workspaceID)()
	rec, err := r.store.WorkspaceReclaim(ctx, workspaceID)
	if errors.Is(err, store.ErrNotFound) {
		return Outcome{Workspace: WorkspaceNone}, nil
	}
	if err != nil {
		return Outcome{}, err
	}
	switch rec.Status {
	case store.ReclaimReclaimed:
		return Outcome{Workspace: OutcomeReclaimed, Reason: rec.Reason, Bytes: rec.RemovedBytes, Files: rec.RemovedFiles}, nil
	case store.ReclaimKept:
		return Outcome{Workspace: OutcomeKept, Reason: rec.Reason}, nil
	}
	id, err := r.store.CheckReclaimIdentity(ctx, rec)
	if err != nil {
		return Outcome{}, err
	}
	switch {
	case !id.Recorded:
		return r.keep(ctx, rec, ReasonIdentityMismatch)
	case id.Shared:
		return r.keep(ctx, rec, ReasonShared)
	}
	var basis store.ReclaimBasis
	if rec.Status == store.ReclaimPending {
		c, err := r.store.ReclaimCheckout(ctx, rec)
		if err != nil {
			return Outcome{}, err
		}
		if c.Delivery.Busy {
			// Both modes wait until the session's delivery work is gone; the next start retries.
			if err := r.store.NoteWorkspaceReclaimAttempt(ctx, workspaceID, ""); err != nil {
				return Outcome{}, err
			}
			return Outcome{Workspace: OutcomePending, Reason: ReasonDelivering}, nil
		}
		if id.Live {
			return r.keep(ctx, rec, ReasonLive)
		}
		var unlock func()
		if rec.Mode == store.ReclaimRecoverable {
			var v Verdict
			if v, unlock, err = r.judge(ctx, c); err != nil {
				if ctx.Err() != nil {
					return Outcome{}, ctx.Err()
				}
				return r.retry(ctx, rec, ReasonCheckFailed, err)
			}
			if v.Workspace == WorkspaceUndelivered {
				if unlock != nil {
					unlock()
				}
				return r.keep(ctx, rec, v.Reason)
			}
			basis = v.basis
		} else {
			// Discarding needs no judgement, but still excludes a delivery of the checkout.
			if unlock, err = r.inspect.LockWorkspace(ctx, rec.Path); err != nil {
				return Outcome{}, err
			}
			basis = store.ReclaimBasis{DeliveryID: c.Delivery.GoodDeliveryID, DeliveryCommit: c.Delivery.GoodCommit, BaseCommit: c.BaseCommit}
		}
		if unlock != nil {
			defer unlock()
		}
		if _, err := r.store.MarkWorkspaceDeleting(ctx, workspaceID, basis); err != nil {
			return Outcome{}, err
		}
	} else {
		// Deleting: only the identity of the path is checked again, never the recovery conditions.
		if id.Live {
			return r.keep(ctx, rec, ReasonLive)
		}
		unlock, err := r.inspect.LockWorkspace(ctx, rec.Path)
		if err != nil {
			return Outcome{}, err
		}
		defer unlock()
		basis = store.ReclaimBasis{DeliveryID: rec.DeliveryID, DeliveryCommit: rec.DeliveryCommit, BaseCommit: rec.BaseCommit}
	}
	return r.remove(ctx, rec, basis)
}

// remove deletes a deleting reclaim's checkout; the Remover checks the path's identity. The
// caller holds the delivery's lock on the checkout.
func (r *Reclaimer) remove(ctx context.Context, rec store.WorkspaceReclaim, basis store.ReclaimBasis) (Outcome, error) {
	usage, err := r.remover.RemoveCheckout(ctx, rec.ProjectID, rec.WorkspaceID, rec.Path)
	switch {
	case errors.Is(err, workspacefs.ErrUnsafeCheckout):
		slog.Warn("workspace reclaim refused an unsafe path", "workspace", rec.WorkspaceID, "path", rec.Path)
		return r.keep(ctx, rec, ReasonUnsafe)
	case errors.Is(err, workspacefs.ErrCheckoutInUse):
		return r.keep(ctx, rec, ReasonLive)
	case err != nil:
		return r.retry(ctx, rec, ReasonRemovalFailed, err)
	}
	basis.Bytes, basis.Files = usage.Bytes, usage.Files
	if err := r.store.FinishWorkspaceReclaimed(ctx, rec.WorkspaceID, basis); err != nil {
		return Outcome{}, err
	}
	slog.Info("reclaimed deleted conversation's workspace", "workspace", rec.WorkspaceID, "conversation", rec.ConversationID,
		"mode", rec.Mode, "reason", basis.Reason, "bytes", usage.Bytes, "files", usage.Files)
	return Outcome{Workspace: OutcomeReclaimed, Reason: basis.Reason, Bytes: usage.Bytes, Files: usage.Files}, nil
}

// retry records a failed attempt that a later start retries: the reclaim keeps its status.
func (r *Reclaimer) retry(ctx context.Context, rec store.WorkspaceReclaim, reason string, cause error) (Outcome, error) {
	slog.Warn("workspace reclaim failed; retried at the next start", "workspace", rec.WorkspaceID,
		"attempts", rec.Attempts+1, "reason", reason, "error", cause)
	// Recorded even when the attempt failed because the server is stopping.
	note, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if note := r.store.NoteWorkspaceReclaimAttempt(note, rec.WorkspaceID, cause.Error()); note != nil {
		return Outcome{}, errors.Join(cause, note)
	}
	return Outcome{Workspace: OutcomePending, Reason: reason}, nil
}

func (r *Reclaimer) keep(ctx context.Context, rec store.WorkspaceReclaim, reason string) (Outcome, error) {
	if err := r.store.FinishWorkspaceKept(ctx, rec.WorkspaceID, reason); err != nil {
		return Outcome{}, err
	}
	slog.Info("kept deleted conversation's workspace", "workspace", rec.WorkspaceID, "path", rec.Path, "reason", reason)
	return Outcome{Workspace: OutcomeKept, Reason: reason}, nil
}

// Resume executes every open reclaim. The server calls it at startup through boot.Recover. A
// reclaim that cannot finish now stays open for the next start and does not stop this one.
func (r *Reclaimer) Resume(ctx context.Context) error {
	open, err := r.store.WorkspaceReclaims(ctx, store.ReclaimPending, store.ReclaimDeleting)
	if err != nil {
		return err
	}
	for _, rec := range open {
		if _, err := r.Reclaim(ctx, rec.WorkspaceID); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("workspace reclaim stays open", "workspace", rec.WorkspaceID, "error", err)
		}
	}
	return nil
}

// DiscardKept removes a kept reclaim's checkout for an operator. The server must not run: the
// caller holds the data directory lock. The path's identity is checked as for any removal.
func (r *Reclaimer) DiscardKept(ctx context.Context, workspaceID string) (Outcome, error) {
	defer r.lock(workspaceID)()
	rec, err := r.store.WorkspaceReclaim(ctx, workspaceID)
	if err != nil {
		return Outcome{}, err
	}
	if rec.Status != store.ReclaimKept {
		return Outcome{}, fmt.Errorf("workspace reclaim %s is %s, not kept", workspaceID, rec.Status)
	}
	id, err := r.store.CheckReclaimIdentity(ctx, rec)
	if err != nil {
		return Outcome{}, err
	}
	if !id.Recorded || id.Shared || id.Live {
		return Outcome{}, fmt.Errorf("workspace reclaim %s: the recorded path is no longer this conversation's alone", workspaceID)
	}
	usage, err := r.remover.RemoveCheckout(ctx, rec.ProjectID, rec.WorkspaceID, rec.Path)
	if err != nil {
		return Outcome{}, err
	}
	basis := store.ReclaimBasis{Reason: ReasonOperatorDiscard, Bytes: usage.Bytes, Files: usage.Files}
	if err := r.store.FinishWorkspaceReclaimed(ctx, workspaceID, basis); err != nil {
		return Outcome{}, err
	}
	return Outcome{Workspace: OutcomeReclaimed, Reason: ReasonOperatorDiscard, Bytes: usage.Bytes, Files: usage.Files}, nil
}
