package workspacefs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

// What in an Agent's repository a delivery cannot carry (InspectReclaim's GitState).
const (
	GitStateStash     = "stash"      // refs/stash exists
	GitStateRef       = "ref"        // a local ref outside refs/remotes points beyond the base and HEAD
	GitStateGitlink   = "gitlink"    // the snapshot has a gitlink that differs from the base
	GitStateNestedGit = "nested_git" // a .git below the checkout's top level
)

// MaxReclaimChanges bounds the undelivered changes InspectReclaim lists.
const MaxReclaimChanges = 200

// ReclaimInspection is what a checkout still holds beyond one of its deliveries.
type ReclaimInspection struct {
	// Tree is the checkout's snapshot tree object, which binds its content (not counting ignored
	// files) for a revision.
	Tree string
	// Unchanged reports that the checkout's snapshot tree, built by the delivery rules, equals
	// the tree it was compared with.
	Unchanged bool
	// GitState names the first thing in the Agent's repository a delivery cannot carry; empty
	// when there is none.
	GitState string
	// Ignored reports files the ignore rules (.gitignore and .git/info/exclude) leave out of
	// every delivery, not counting the skills Ora materialized.
	Ignored bool
	// Changes lists, up to MaxReclaimChanges, how the snapshot differs from the compared tree;
	// Total counts all of them.
	Changes []Change
	Total   int
}

// InspectReclaim tells what removing the checkout would lose beyond the delivery commit against
// (the base commit when against is empty). It builds the snapshot tree as a delivery would, but
// only in scratch space: nothing reaches the mirror or a delivery ref. Every check is read-only and
// runs with the delivery's hardened Git configuration. The caller must hold the checkout lock.
// ErrUnverifiable means what the checkout holds cannot be verified; any other error (the disk,
// the mirror, Git) may pass on a later attempt.
func (s *DeliveryFS) InspectReclaim(ctx context.Context, ws Workspace, mirrorPath, against string) (ReclaimInspection, error) {
	var out ReclaimInspection
	st, err := s.stage(ctx, ws, mirrorPath, time.Time{})
	if errors.Is(err, ErrMaterializedUnknown) || errors.Is(err, ErrDeliveryTooLarge) || errors.Is(err, ErrTooLarge) {
		err = fmt.Errorf("%w: %w", ErrUnverifiable, err)
	}
	if err != nil {
		return out, err
	}
	out.Tree = st.tree
	defer st.close()
	target := st.baseTree
	if against != "" {
		if !validCommit(against) {
			return out, errors.New("invalid delivery commit")
		}
		if target, err = st.mirror.value(ctx, "rev-parse", "--verify", against+"^{tree}"); err != nil {
			return out, err
		}
	}
	out.Unchanged = st.tree == target
	if !out.Unchanged {
		status, err := st.git.run(ctx, nil, MaxDiffBytes, "diff-tree", "-r", "--no-renames", "--name-status", "-z", target, st.tree)
		if err != nil {
			return out, err
		}
		stats, err := st.git.run(ctx, nil, MaxDiffBytes, "diff-tree", "-r", "--no-renames", "--numstat", "-z", target, st.tree)
		if err != nil {
			return out, err
		}
		var all Changes
		populateChanges(&all, status, stats)
		out.Total = strings.Count(string(status), "\x00") / 2
		out.Changes = all.Files[:min(len(all.Files), MaxReclaimChanges)]
	}
	if out.GitState, err = s.gitState(ctx, st); err != nil {
		return out, err
	}
	out.Ignored, err = ignoredFiles(ctx, st)
	return out, err
}

// gitState looks for work only the checkout's repository holds: a stash, local refs beyond the
// base and HEAD, a changed gitlink in the snapshot, or a nested repository.
func (s *DeliveryFS) gitState(ctx context.Context, st *staged) (string, error) {
	raw, err := st.git.run(ctx, nil, MaxDiffBytes, "diff-tree", "-r", "--no-renames", "-z", st.baseTree, st.tree)
	if err != nil {
		return "", err
	}
	for _, field := range strings.Split(string(raw), "\x00") {
		// Raw entries start ":<old mode> <new mode> ..."; a gitlink has mode 160000.
		if modes := strings.Fields(strings.TrimPrefix(field, ":")); strings.HasPrefix(field, ":") && len(modes) >= 2 &&
			(modes[0] == "160000" || modes[1] == "160000") {
			return GitStateGitlink, nil
		}
	}
	nested, err := nestedGit(st.root)
	if err != nil || nested {
		return GitStateNestedGit, err
	}
	// The Agent's own repository, found from the checkout the way Git finds it, read with the
	// hardened configuration only. Without one there are no refs to lose.
	if _, err := os.Lstat(st.root + "/.git"); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	agent := deliveryGit{binary: s.git, dir: st.root, env: []string{"GIT_CEILING_DIRECTORIES=" + path.Dir(st.root)}}
	refs, err := agent.run(ctx, nil, MaxDiffBytes, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return "", err
	}
	var local []string
	for _, ref := range strings.Split(strings.TrimSpace(string(refs)), "\n") {
		switch {
		case ref == "":
		case ref == "refs/stash":
			return GitStateStash, nil
		case !strings.HasPrefix(ref, "refs/remotes/"):
			local = append(local, ref)
		}
	}
	if len(local) == 0 {
		return "", nil
	}
	args := append([]string{"rev-list", "-n1"}, local...)
	args = append(args, "--not", st.base)
	if _, err := agent.value(ctx, "rev-parse", "--verify", "-q", "HEAD"); err == nil {
		args = append(args, "HEAD")
	}
	beyond, err := agent.value(ctx, append(args, "--")...)
	if err != nil {
		return "", err
	}
	if beyond != "" {
		return GitStateRef, nil
	}
	return "", nil
}

// nestedGit reports a .git anywhere below the checkout's top level, without following links.
func nestedGit(root string) (bool, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return false, err
	}
	defer r.Close()
	found := false
	err = fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == ".git" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() == ".git" {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found, err
}

// ignoredFiles reports files the snapshot leaves out by the ignore rules, apart from the skills
// Ora materialized (which those rules exclude on purpose).
func ignoredFiles(ctx context.Context, st *staged) (bool, error) {
	raw, err := st.git.run(ctx, nil, 1<<20, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	if errors.Is(err, ErrTooLarge) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, p := range bytes.Split(raw, []byte{0}) {
		name := strings.TrimSuffix(string(p), "/")
		if name == "" {
			continue
		}
		materialized := false
		for _, m := range append(materializedRoots, ".opencode", ".claude") {
			if name == m || strings.HasPrefix(name, m+"/") {
				materialized = true
			}
		}
		if !materialized {
			return true, nil
		}
	}
	return false, nil
}
