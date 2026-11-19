//go:build linux

package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/wanglongan587/cloud/v2-server/internal/workspacefs"
)

// removeCheckoutDir removes the directory; tests replace it to hold a removal open.
var removeCheckoutDir = workspacefs.RemoveCheckout

// projectLock is the lock that stands in for a project's skillsMu while the project has no
// runtime: d.project takes it before inserting a new projectRuntime, so a runtime cannot appear
// while its skill list is rewritten. Locks stay for the Driver's life; there is one per project.
func (d *Driver) projectLock(id string) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.projectLocks == nil {
		d.projectLocks = map[string]*sync.Mutex{}
	}
	l := d.projectLocks[id]
	if l == nil {
		l = &sync.Mutex{}
		d.projectLocks[id] = l
	}
	return l
}

// RemoveCheckout deletes a workspace's checkout of the project, for a conversation the person
// deleted. It first detaches everything this process holds for the checkout, briefly: under the
// project's skillsMu (or, without a runtime, the project lock) it refuses with
// workspacefs.ErrCheckoutInUse a checkout a live session is admitted to, otherwise drops it from
// p.checkouts (under p.mu), from the reattach records and from skill-checkouts.json, and releases
// the locks. Measuring and removing then run without them, so sessions of the project start
// meanwhile; the caller holds the delivery's lock on the checkout (lock order: LockWorkspace,
// skillsMu, p.mu). workspacefs.ErrUnsafeCheckout means the recorded path is not one to delete.
func (d *Driver) RemoveCheckout(ctx context.Context, projectID, workspaceID, path string) (workspacefs.Usage, error) {
	if projectID == "" || workspaceID == "" {
		return workspacefs.Usage{}, workspacefs.ErrUnsafeCheckout
	}
	if err := d.detachCheckout(projectID, workspaceID, path); err != nil {
		return workspacefs.Usage{}, err
	}
	root := filepath.Join(d.opts.Root, digest(projectID)[:16])
	return removeCheckoutDir(ctx, filepath.Join(root, "w", "r"), path)
}

func (d *Driver) detachCheckout(projectID, workspaceID, path string) error {
	root := filepath.Join(d.opts.Root, digest(projectID)[:16])
	lock := d.projectLock(projectID)
	lock.Lock()
	d.mu.Lock()
	p := d.projects[projectID]
	delete(d.recorded, workspaceID)
	d.mu.Unlock()
	if p == nil {
		defer lock.Unlock()
		return forgetSkillCheckout(root, path)
	}
	// The runtime exists, so its skillsMu guards the list; the project lock is not needed.
	lock.Unlock()
	p.skillsMu.Lock()
	defer p.skillsMu.Unlock()
	p.mu.Lock()
	if len(p.owners[workspaceID]) > 0 {
		p.mu.Unlock()
		return workspacefs.ErrCheckoutInUse
	}
	delete(p.checkouts, workspaceID)
	p.mu.Unlock()
	return forgetSkillCheckout(root, path)
}

// forgetSkillCheckout drops path from the project's skill-checkouts.json, the checkouts market
// skill projection walks. The caller holds the lock that serializes writes to it.
func forgetSkillCheckout(root, path string) error {
	index := filepath.Join(root, "skill-checkouts.json")
	raw, err := os.ReadFile(index)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read skill checkouts: %w", err)
	}
	var known []string
	if err := json.Unmarshal(raw, &known); err != nil {
		return fmt.Errorf("read skill checkouts: %w", err)
	}
	kept := slices.DeleteFunc(slices.Clone(known), func(k string) bool { return k == path })
	if len(kept) == len(known) {
		return nil
	}
	if kept == nil {
		kept = []string{}
	}
	return writeSkillIndex(index, kept)
}

// OfflineCheckouts returns a Driver over the Agent runtime root (V2_DATA_DIR/nodes) that serves
// only RemoveCheckout, for an operator command run while the server is stopped and the data
// directory lock is held: no project runtime exists, so only the on-disk skill index is updated.
func OfflineCheckouts(root string) *Driver {
	return &Driver{opts: Options{Root: root}, projects: map[string]*projectRuntime{}, recorded: map[string]Checkout{}}
}
