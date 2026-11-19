package workspacefs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/gitexclude"
)

// GitFindings is the work only the checkout's repository holds, which no delivery carries.
type GitFindings struct {
	// Stashes counts the entries of refs/stash.
	Stashes int `json:"stashes"`
	// Refs are local refs (outside refs/remotes/) beyond the base and HEAD, or not commits.
	Refs []RefFinding `json:"refs"`
	// Repositories are nested repositories: a .git of any kind below the top level, a gitlink the
	// snapshot changed, or an initialized submodule under .git/modules/.
	Repositories []string `json:"repositories"`
}

// RefFinding is one ref that points at work beyond the base and HEAD.
type RefFinding struct {
	Name string `json:"name"`
	// Commits counts the commits it reaches that neither the base nor HEAD reaches.
	Commits int `json:"commits"`
	// NotCommit reports a ref that does not peel to a commit (a tree, a blob, a tag of either).
	NotCommit bool `json:"notCommit,omitempty"`
}

// Empty reports that the repository holds nothing beyond the snapshot.
func (g GitFindings) Empty() bool {
	return g.Stashes == 0 && len(g.Refs) == 0 && len(g.Repositories) == 0
}

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
	// Git is what the Agent's repository holds that a delivery cannot carry.
	Git GitFindings
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
	if out.Git, err = s.gitState(ctx, st); err != nil {
		return out, err
	}
	out.Ignored, err = ignoredFiles(ctx, st)
	return out, err
}

// gitState lists work only the checkout's repository holds: a stash, local refs beyond the base
// and HEAD (or not commits), changed gitlinks in the snapshot, nested repositories of any kind
// (ignored directories included) and initialized submodules. A .git that cannot be resolved makes
// the checkout unverifiable; one that is absent has no refs to lose.
func (s *DeliveryFS) gitState(ctx context.Context, st *staged) (GitFindings, error) {
	g := GitFindings{Refs: []RefFinding{}, Repositories: []string{}}
	raw, err := st.git.run(ctx, nil, MaxDiffBytes, "diff-tree", "-r", "--no-renames", "-z", st.baseTree, st.tree)
	if err != nil {
		return g, err
	}
	fields := strings.Split(string(raw), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		// Raw entries are ":<old mode> <new mode> ..." then the path; a gitlink has mode 160000.
		if modes := strings.Fields(strings.TrimPrefix(fields[i], ":")); len(modes) >= 2 && (modes[0] == "160000" || modes[1] == "160000") {
			g.Repositories = append(g.Repositories, fields[i+1])
		}
	}
	nested, err := nestedGit(st.root)
	if err != nil {
		return g, err
	}
	g.Repositories = append(g.Repositories, nested...)
	layout, err := gitexclude.Resolve(st.root)
	if errors.Is(err, fs.ErrNotExist) {
		if _, lerr := os.Lstat(filepath.Join(st.root, ".git")); errors.Is(lerr, fs.ErrNotExist) {
			return g, nil
		}
	}
	if err != nil {
		return g, fmt.Errorf("%w: %w", ErrUnverifiable, err)
	}
	modules, err := os.ReadDir(filepath.Join(layout.CommonDir(), "modules"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return g, err
	}
	for _, m := range modules {
		g.Repositories = append(g.Repositories, ".git/modules/"+m.Name())
	}
	// The Agent's own repository, found from the checkout the way Git finds it, with hooks,
	// fsmonitor, filters and global or system configuration overridden on the command line.
	agent := deliveryGit{binary: s.git, dir: st.root, env: []string{"GIT_CEILING_DIRECTORIES=" + filepath.Dir(st.root)}}
	refs, err := agent.run(ctx, nil, MaxDiffBytes, "for-each-ref", "--format=%(refname)%00%(objecttype)%00%(*objecttype)")
	if err != nil {
		return g, err
	}
	stop := []string{"--not", st.base}
	if _, err := agent.value(ctx, "rev-parse", "--verify", "-q", "HEAD^{commit}"); err == nil {
		stop = append(stop, "HEAD")
	}
	for _, line := range strings.Split(strings.TrimSpace(string(refs)), "\n") {
		parts := strings.Split(line, "\x00")
		if len(parts) != 3 || parts[0] == "" || strings.HasPrefix(parts[0], "refs/remotes/") {
			continue
		}
		name, kind, peeled := parts[0], parts[1], parts[2]
		if name == "refs/stash" {
			count, err := agent.value(ctx, "rev-list", "--walk-reflogs", "--count", "refs/stash")
			n, convErr := strconv.Atoi(count)
			if err != nil || convErr != nil || n < 1 {
				n = 1
			}
			g.Stashes = n
			continue
		}
		if kind != "commit" && !(kind == "tag" && peeled == "commit") {
			g.Refs = append(g.Refs, RefFinding{Name: name, NotCommit: true})
			continue
		}
		count, err := agent.value(ctx, append(append([]string{"rev-list", "--count", name + "^{commit}"}, stop...), "--")...)
		if err != nil {
			return g, err
		}
		if n, err := strconv.Atoi(count); err == nil && n > 0 {
			g.Refs = append(g.Refs, RefFinding{Name: name, Commits: n})
		}
	}
	// A submodule is both a changed gitlink and a nested .git: name it once.
	seen := map[string]bool{}
	g.Repositories = slices.DeleteFunc(g.Repositories, func(p string) bool {
		dup := seen[p]
		seen[p] = true
		return dup
	})
	return g, nil
}

// nestedGit lists every entry named .git below the checkout's top level, whatever its kind
// (directory, gitfile, link), without following links and including ignored directories.
func nestedGit(root string) ([]string, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	found := []string{}
	err = fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() != ".git" || p == "." {
			return nil
		}
		if p != ".git" {
			found = append(found, path.Dir(p))
		}
		if d.IsDir() {
			return fs.SkipDir
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
