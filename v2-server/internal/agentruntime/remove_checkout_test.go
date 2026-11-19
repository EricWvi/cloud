//go:build linux

package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/workspacefs"
)

func removalFixture(t *testing.T) (d *Driver, root, keep, gone string) {
	t.Helper()
	d = &Driver{opts: Options{Root: t.TempDir()}, projects: map[string]*projectRuntime{}, recorded: map[string]Checkout{}}
	root = filepath.Join(d.opts.Root, digest("project")[:16])
	keep = filepath.Join(root, "w", "r", "keep")
	gone = filepath.Join(root, "w", "r", "gone")
	for _, dir := range []string{keep, gone} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("abc"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeSkillIndex(filepath.Join(root, "skill-checkouts.json"), []string{keep, gone}); err != nil {
		t.Fatal(err)
	}
	return d, root, keep, gone
}

func skillCheckouts(t *testing.T, root string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "skill-checkouts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var known []string
	if err := json.Unmarshal(raw, &known); err != nil {
		t.Fatal(err)
	}
	return known
}

func TestRemoveCheckoutWithoutRuntimeForgetsSkillIndex(t *testing.T) {
	d, root, keep, gone := removalFixture(t)
	d.recorded["ws"] = Checkout{Path: gone, CloneExecution: "c-1"}
	usage, err := d.RemoveCheckout(t.Context(), "project", "ws", gone)
	if err != nil {
		t.Fatal(err)
	}
	if usage != (workspacefs.Usage{Bytes: 3, Files: 1}) {
		t.Fatalf("usage = %+v", usage)
	}
	if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkout still there: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("sibling touched: %v", err)
	}
	if got := skillCheckouts(t, root); !slices.Equal(got, []string{keep}) {
		t.Fatalf("skill checkouts = %v", got)
	}
	if _, ok := d.recorded["ws"]; ok {
		t.Fatal("reattach record kept")
	}
}

func TestRemoveCheckoutRefusesAdmittedSessionAndDropsCachedCheckout(t *testing.T) {
	d, root, keep, gone := removalFixture(t)
	p := &projectRuntime{driver: d, id: "project", root: root,
		checkouts: map[string]*checkout{"ws": {path: gone}},
		owners:    map[string]map[string]*execution.Skills{"ws": {"session": nil}}}
	d.projects["project"] = p
	if _, err := d.RemoveCheckout(t.Context(), "project", "ws", gone); !errors.Is(err, workspacefs.ErrCheckoutInUse) {
		t.Fatalf("err = %v, want workspacefs.ErrCheckoutInUse", err)
	}
	if _, err := os.Stat(gone); err != nil {
		t.Fatalf("in-use checkout removed: %v", err)
	}
	if got := skillCheckouts(t, root); !slices.Equal(got, []string{keep, gone}) {
		t.Fatalf("skill checkouts changed while in use: %v", got)
	}
	delete(p.owners, "ws")
	if _, err := d.RemoveCheckout(t.Context(), "project", "ws", gone); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.checkouts["ws"]; ok {
		t.Fatal("cached checkout kept")
	}
	if got := skillCheckouts(t, root); !slices.Equal(got, []string{keep}) {
		t.Fatalf("skill checkouts = %v", got)
	}
}

func TestRemoveCheckoutRefusesPathOfAnotherProject(t *testing.T) {
	d, _, _, gone := removalFixture(t)
	if _, err := d.RemoveCheckout(t.Context(), "other-project", "ws", gone); !errors.Is(err, workspacefs.ErrUnsafeCheckout) {
		t.Fatalf("err = %v, want ErrUnsafeCheckout", err)
	}
	if _, err := os.Stat(gone); err != nil {
		t.Fatalf("checkout of another project removed: %v", err)
	}
}

// A checkout reclaimed after projection listed it is skipped; the others still get their links.
func TestProjectSkillsSkipsAVanishedCheckout(t *testing.T) {
	live := gitCheckout(t)
	gone := filepath.Join(t.TempDir(), "reclaimed")
	p := &projectRuntime{root: t.TempDir(), driver: &Driver{opts: Options{}}}
	link := skillLink{ID: "acme/tools", Version: "1.0.0", Name: "lint"}
	if err := os.MkdirAll(p.assetPath(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := p.projectSkills([]string{gone, live}, nil, []skillLink{link}); err != nil {
		t.Fatalf("projection failed on a vanished checkout: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(live, ".claude", "skills", "lint")); err != nil {
		t.Fatalf("the live checkout did not get its link: %v", err)
	}
}

// While a removal measures and deletes, the project's skill lock is free: sessions of the same
// project can attach skills (and start) instead of waiting for a large directory to go.
func TestRemoveCheckoutDoesNotHoldTheSkillLockWhileDeleting(t *testing.T) {
	d, root, keep, gone := removalFixture(t)
	p := &projectRuntime{driver: d, id: "project", root: root, checkouts: map[string]*checkout{}, owners: map[string]map[string]*execution.Skills{}}
	d.projects["project"] = p
	deleting, release := make(chan struct{}), make(chan struct{})
	removeCheckoutDir = func(ctx context.Context, root, path string) (workspacefs.Usage, error) {
		close(deleting)
		<-release
		return workspacefs.RemoveCheckout(ctx, root, path)
	}
	defer func() { removeCheckoutDir = workspacefs.RemoveCheckout }()
	done := make(chan error, 1)
	go func() {
		_, err := d.RemoveCheckout(t.Context(), "project", "ws", gone)
		done <- err
	}()
	<-deleting
	attached := make(chan error, 1)
	go func() { attached <- p.attachSkills(keep) }()
	select {
	case err := <-attached:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attaching skills waited for the checkout removal")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Without a runtime the removal holds the project lock while it rewrites the skill list, and a
// runtime being created for the project waits for it.
func TestProjectRuntimeCreationWaitsForTheSkillListRewrite(t *testing.T) {
	d, _, _, _ := removalFixture(t)
	lock := d.projectLock("project")
	lock.Lock()
	created := make(chan struct{})
	d.startProject = func(*projectRuntime) error { return errors.New("not started in this test") }
	go func() {
		_, _ = d.project(t.Context(), "project", Project{})
		close(created)
	}()
	time.Sleep(50 * time.Millisecond)
	d.mu.Lock()
	_, exists := d.projects["project"]
	d.mu.Unlock()
	if exists {
		t.Fatal("a runtime was inserted while the project lock was held")
	}
	lock.Unlock()
	select {
	case <-created:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime creation never proceeded")
	}
}
