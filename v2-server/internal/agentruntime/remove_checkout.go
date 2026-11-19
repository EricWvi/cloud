//go:build linux

package agentruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/wanglongan587/cloud/v2-server/internal/workspacefs"
)

// RemoveCheckout deletes a workspace's checkout of the project, for a conversation the person
// deleted. It first detaches everything this process still holds for the checkout: a session
// admitted to it refuses the removal with workspacefs.ErrCheckoutInUse; otherwise the cached checkout, the
// reattach record and the skill projection's list of checkouts drop it, the list under the same
// lock projection holds, so a projection never meets a checkout that vanished halfway. The
// directory itself is removed by workspacefs.RemoveCheckout inside the project's checkout root;
// ErrUnsafeCheckout from it means the recorded path is not one this process may delete.
func (d *Driver) RemoveCheckout(projectID, workspaceID, path string) (workspacefs.Usage, error) {
	if projectID == "" || workspaceID == "" {
		return workspacefs.Usage{}, workspacefs.ErrUnsafeCheckout
	}
	root := filepath.Join(d.opts.Root, digest(projectID)[:16])
	// d.mu is held while the project is looked up and, when it has no runtime, while its skill
	// list is rewritten: a runtime created meanwhile would otherwise attach a checkout to the list
	// that this rewrite then loses.
	d.mu.Lock()
	p := d.projects[projectID]
	delete(d.recorded, workspaceID)
	if p == nil {
		err := forgetSkillCheckout(root, path)
		d.mu.Unlock()
		if err != nil {
			return workspacefs.Usage{}, err
		}
		return workspacefs.RemoveCheckout(filepath.Join(root, "w", "r"), path)
	}
	d.mu.Unlock()
	p.skillsMu.Lock()
	p.mu.Lock()
	if len(p.owners[workspaceID]) > 0 {
		p.mu.Unlock()
		p.skillsMu.Unlock()
		return workspacefs.Usage{}, workspacefs.ErrCheckoutInUse
	}
	delete(p.checkouts, workspaceID)
	p.mu.Unlock()
	err := forgetSkillCheckout(root, path)
	p.skillsMu.Unlock()
	if err != nil {
		return workspacefs.Usage{}, err
	}
	return workspacefs.RemoveCheckout(filepath.Join(root, "w", "r"), path)
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
