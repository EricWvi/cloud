package workspacefs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
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

// inspectTimeout bounds one InspectReclaim.
const inspectTimeout = 2 * time.Minute

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
	// Git is what the Agent's repository holds that a delivery cannot carry, as counts to show.
	Git GitFindings
	// GitIdentity digests the actual Git state behind Git (stash entries, ref targets, HEAD,
	// nested repositories), so a revision changes when that state does even if the counts do not.
	GitIdentity string
	// GitIdentityComplete is false when a nested repository was too large to digest in full: the
	// identity then cannot vouch for what discarding the checkout would drop.
	GitIdentityComplete bool
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
	// Git invoked on a checkout can wait on whatever the checkout holds (a FIFO where Git expects a
	// file); bound the whole inspection like a delivery, so a startup never hangs on one checkout.
	ctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
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
	if out.GitIdentity, out.GitIdentityComplete, err = s.gitIdentity(ctx, st, out.Git); err != nil {
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

// Limits of the identity digest of nested repositories. maxGitMetadataBytes bounds one file of a
// repository's state (HEAD, packed-refs, a ref, the stash log); maxGitfileBytes a gitfile;
// maxNestedEntries the entries of nested working trees. Their file contents share the delivery's
// object limit. Beyond a limit the digest is incomplete and a discard cannot be verified.
const (
	maxGitMetadataBytes = 8 << 20
	maxGitfileBytes     = 4 << 10
	maxNestedEntries    = 100000
)

// gitIdentity digests what identifies the Git state gitState counted: every ref's name and
// target (peeled), HEAD and each stash reflog entry of the checkout's repository; and for every
// nested repository the files that name its state (HEAD, packed-refs, refs/, the stash log, the
// index's size and time) or its gitfile, plus its working tree by content: these files are in no
// snapshot, so the digest must change whenever any of them does. Files are read without Git,
// without following links, after an Lstat shows a regular file and an fstat confirms it, and opened
// non-blocking, so a FIFO or a device cannot hang the read. A special file where Git metadata
// belongs makes the checkout unverifiable. complete is false when a limit cut the digest short:
// the identity then cannot vouch for what a discard would drop.
func (s *DeliveryFS) gitIdentity(ctx context.Context, st *staged, g GitFindings) (identity string, complete bool, err error) {
	h := sha256.New()
	if _, err := os.Lstat(filepath.Join(st.root, ".git")); err == nil {
		agent := deliveryGit{binary: s.git, dir: st.root, env: []string{"GIT_CEILING_DIRECTORIES=" + filepath.Dir(st.root)}}
		refs, err := agent.run(ctx, nil, MaxDiffBytes, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(*objectname)")
		if err != nil {
			return "", false, err
		}
		h.Write(refs)
		head, _ := agent.value(ctx, "rev-parse", "-q", "--verify", "HEAD")
		fmt.Fprintf(h, "HEAD %s\x00", head)
		if g.Stashes > 0 {
			stashes, err := agent.run(ctx, nil, MaxDiffBytes, "rev-list", "--walk-reflogs", "refs/stash")
			if err != nil {
				return "", false, err
			}
			h.Write(stashes)
		}
	}
	root, err := os.OpenRoot(st.root)
	if err != nil {
		return "", false, err
	}
	defer root.Close()
	d := &digester{ctx: ctx, h: h, bytes: s.maxObjects, complete: true}
	for _, repo := range g.Repositories {
		fmt.Fprintf(h, "repository %s\x00", repo)
		if name, ok := strings.CutPrefix(repo, ".git/modules/"); ok {
			layout, err := gitexclude.Resolve(st.root)
			if err != nil {
				return "", false, fmt.Errorf("%w: %w", ErrUnverifiable, err)
			}
			modules, err := os.OpenRoot(filepath.Join(layout.CommonDir(), "modules"))
			if err != nil {
				return "", false, err
			}
			err = d.gitDir(modules, name)
			modules.Close()
			if err != nil {
				return "", false, err
			}
			continue
		}
		gitEntry := path.Join(repo, ".git")
		if info, err := root.Lstat(gitEntry); err == nil {
			switch {
			case info.IsDir():
				if err := d.gitDir(root, gitEntry); err != nil {
					return "", false, err
				}
			case info.Mode()&fs.ModeSymlink != 0:
				target, err := root.Readlink(gitEntry)
				if err != nil {
					return "", false, err
				}
				fmt.Fprintf(h, "link %s\x00", target)
			case info.Mode().IsRegular() && info.Size() <= maxGitfileBytes:
				if err := d.file(root, gitEntry, maxGitfileBytes, true); err != nil {
					return "", false, err
				}
			default:
				return "", false, fmt.Errorf("%w: nested %s is not a regular gitfile", ErrUnverifiable, gitEntry)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", false, err
		}
		if err := d.tree(root, repo); err != nil {
			return "", false, err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), d.complete, nil
}

// digester writes file identities into h within a budget of content bytes and entries.
type digester struct {
	ctx      context.Context
	h        io.Writer
	bytes    int64
	entries  int
	complete bool
}

// file writes the content of name, which must be a regular file of at most limit bytes (a
// metadata file is an error past it when strict, and only ends the digest otherwise); a missing
// file writes nothing. It never follows a link and never blocks on a special file.
func (d *digester) file(r *os.Root, name string, limit int64, strict bool) error {
	info, err := r.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(d.h, "%s -\x00", name)
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrUnverifiable, name)
	}
	if info.Size() > limit || info.Size() > d.bytes {
		if strict {
			return fmt.Errorf("%w: %s exceeds its size limit", ErrUnverifiable, name)
		}
		d.complete = false
		return nil
	}
	f, err := r.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return fmt.Errorf("%w: %s changed while it was read", ErrUnverifiable, name)
	}
	fmt.Fprintf(d.h, "%s %d\x00", name, opened.Size())
	n, err := io.Copy(d.h, io.LimitReader(f, min(limit, d.bytes)+1))
	if err != nil {
		return err
	}
	if n > min(limit, d.bytes) {
		if strict {
			return fmt.Errorf("%w: %s grew past its size limit", ErrUnverifiable, name)
		}
		d.complete = false
		return nil
	}
	d.bytes -= n
	return nil
}

// gitDir writes the files that name a repository's state: HEAD, packed-refs, every file under
// refs/, the stash log, and the index's size and modification time.
func (d *digester) gitDir(r *os.Root, dir string) error {
	for _, name := range []string{"HEAD", "packed-refs", "logs/refs/stash"} {
		if err := d.file(r, path.Join(dir, name), maxGitMetadataBytes, true); err != nil {
			return err
		}
	}
	if info, err := r.Lstat(path.Join(dir, "index")); err == nil {
		fmt.Fprintf(d.h, "index %s %d %d\x00", info.Mode().Type(), info.Size(), info.ModTime().UnixNano())
	}
	err := fs.WalkDir(r.FS(), path.Join(dir, "refs"), func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := d.ctx.Err(); err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		return d.file(r, p, maxGitMetadataBytes, true)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// tree writes every entry of a nested working tree (its .git apart): regular files by content,
// links by target, other entries by type, all without following links.
func (d *digester) tree(r *os.Root, dir string) error {
	if _, err := r.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil // a gitlink with no working tree here
	}
	return fs.WalkDir(r.FS(), dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := d.ctx.Err(); err != nil {
			return err
		}
		if e.Name() == ".git" && p != dir {
			if e.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.entries++; d.entries > maxNestedEntries {
			d.complete = false
			return fs.SkipAll
		}
		switch t := e.Type(); {
		case t.IsDir():
			fmt.Fprintf(d.h, "dir %s\x00", p)
		case t&fs.ModeSymlink != 0:
			target, err := r.Readlink(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(d.h, "link %s %s\x00", p, target)
		case t.IsRegular():
			if err := d.file(r, p, d.bytes, false); err != nil {
				return err
			}
			if !d.complete {
				return fs.SkipAll
			}
		default:
			fmt.Fprintf(d.h, "special %s %s\x00", p, t)
		}
		return nil
	})
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
