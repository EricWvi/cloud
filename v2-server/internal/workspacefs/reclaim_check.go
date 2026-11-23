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
	// HeadDiffers reports a HEAD beyond the base whose tree differs from the delivery's on a path
	// it tracks; HeadCommits counts its commits beyond the base.
	HeadDiffers bool `json:"headDiffers"`
	HeadCommits int  `json:"headCommits,omitempty"`
	// IndexDiffers reports an index with a conflict, or an entry whose blob or mode differs from
	// the delivery's at the same path.
	IndexDiffers bool `json:"indexDiffers"`
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
	return g.Stashes == 0 && len(g.Refs) == 0 && len(g.Repositories) == 0 && !g.HeadDiffers && !g.IndexDiffers
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
	// GitIdentityComplete is false when the identity could not bind every byte a discard would
	// delete: a size or entry limit, a special file, a failed read, a pointer leading outside the
	// checkout or not resolving, or a symlink, special file, or objects/info/alternates inside a
	// nested Git directory. The identity then cannot vouch for what discarding the checkout would
	// drop, so a discard is refused.
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
	if out.Git, err = s.gitState(ctx, st, target); err != nil {
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
func (s *DeliveryFS) gitState(ctx context.Context, st *staged, target string) (GitFindings, error) {
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
	// Malformed ref files first: Git may fail on them with a message that says nothing of objects.
	if err := checkRecordedRefs(layout, nil); err != nil {
		return g, err
	}
	refs, err := agent.run(ctx, nil, MaxDiffBytes, "for-each-ref", "--format=%(refname)%00%(objecttype)%00%(*objecttype)")
	if err != nil {
		return g, corrupt(err)
	}
	// for-each-ref skips a ref it cannot read with a warning and still succeeds, so the refs
	// recorded on disk are checked against what it listed.
	listed := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(refs)), "\n") {
		if name, _, ok := strings.Cut(line, "\x00"); ok {
			listed[name] = true
		}
	}
	if err := checkRecordedRefs(layout, listed); err != nil {
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
			return g, corrupt(err)
		}
		if n, err := strconv.Atoi(count); err == nil && n > 0 {
			g.Refs = append(g.Refs, RefFinding{Name: name, Commits: n})
		}
	}
	if g.HeadDiffers, g.HeadCommits, g.IndexDiffers, err = s.headAndIndex(ctx, agent, st, target); err != nil {
		return g, err
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

// maxNestedEntries bounds the entries the identity digest reads; its file contents share the
// delivery's object limit. Beyond either the digest is incomplete.
const maxNestedEntries = 100000

// gitIdentity digests the actual bytes of everything Git-related that a discard would delete and
// no delivery carries: the checkout repository's refs (name, target, peeled target), HEAD, stash
// reflog entries and index; and for every nested repository its whole subtree (working tree and
// .git, objects included) by content, plus, when its .git is a gitfile or a link, the Git
// directories it points at as long as they lie inside the checkout. A revision built on it
// changes whenever any of those bytes does.
//
// complete reports that every such byte was bound. It is false, and stays false, when anything
// could not be: a byte or entry limit, a special file, a read that failed, a pointer leading out
// of the checkout or not resolving, or any symlink, special file, or objects/info/alternates at
// any depth inside a nested Git directory (including .git/modules entries and resolved Git
// directories). A discard is then
// refused, since the revision cannot prove the person saw what would be deleted. Files are read
// without Git and without following links: an Lstat must show a regular file, the open is
// O_NOFOLLOW|O_NONBLOCK and an fstat must confirm the same file, so nothing can block on a FIFO
// or a device.
func (s *DeliveryFS) gitIdentity(ctx context.Context, st *staged, g GitFindings) (identity string, complete bool, err error) {
	h := sha256.New()
	d := &digester{ctx: ctx, h: h, bytes: s.maxObjects, complete: true}
	if _, err := os.Lstat(filepath.Join(st.root, ".git")); err == nil {
		agent := deliveryGit{binary: s.git, dir: st.root, env: []string{"GIT_CEILING_DIRECTORIES=" + filepath.Dir(st.root)}}
		refs, err := agent.run(ctx, nil, MaxDiffBytes, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(*objectname)")
		if err != nil {
			return "", false, corrupt(err)
		}
		h.Write(refs)
		head, _ := agent.value(ctx, "rev-parse", "-q", "--verify", "HEAD")
		fmt.Fprintf(h, "HEAD %s\x00", head)
		if g.Stashes > 0 {
			stashes, err := agent.run(ctx, nil, MaxDiffBytes, "rev-list", "--walk-reflogs", "refs/stash")
			if err != nil {
				return "", false, corrupt(err)
			}
			h.Write(stashes)
		}
		// The index: what is staged is in no snapshot (the snapshot reads the working tree).
		if layout, err := gitexclude.Resolve(st.root); err != nil {
			d.incomplete("resolve .git", err)
		} else if gitdir, err := os.OpenRoot(layout.GitDir()); err != nil {
			d.incomplete("open Git directory", err)
		} else {
			d.file(gitdir, "index")
			gitdir.Close()
		}
	}
	root, err := os.OpenRoot(st.root)
	if err != nil {
		return "", false, err
	}
	defer root.Close()
	realRoot, err := filepath.EvalSymlinks(st.root)
	if err != nil {
		return "", false, err
	}
	// digested lists the subtrees already bound, so a Git directory several repositories share
	// (or one inside a repository's own subtree) is read once. checkedGitDirs tracks the Git
	// directories already verified to contain no symlinks, special files, or alternates.
	var digested []string
	var checkedGitDirs []string
	for _, repo := range g.Repositories {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		fmt.Fprintf(h, "repository %s\x00", repo)
		if name, ok := strings.CutPrefix(repo, ".git/modules/"); ok {
			layout, err := gitexclude.Resolve(st.root)
			if err != nil {
				d.incomplete("resolve .git", err)
				continue
			}
			modules, err := os.OpenRoot(filepath.Join(layout.CommonDir(), "modules"))
			if err != nil {
				d.incomplete("open modules", err)
				continue
			}
			if _, err := modules.Lstat(path.Join(name, "commondir")); err == nil || !errors.Is(err, fs.ErrNotExist) {
				d.incomplete("commondir inside nested module", err)
			}
			d.gitTree(modules, name)
			modules.Close()
			if real, err := filepath.EvalSymlinks(filepath.Join(layout.CommonDir(), "modules", name)); err == nil {
				if rel, err := filepath.Rel(realRoot, real); err == nil && filepath.IsLocal(rel) && rel != "." {
					rel = filepath.ToSlash(rel)
					digested = append(digested, rel)
					checkedGitDirs = append(checkedGitDirs, rel)
				}
			}
			continue
		}
		// Resolve the repository's Git directories before walking it: a gitfile or a link points at
		// them, and a .git directory may name a shared one in commondir. They are bound once, by the
		// same walk that covers the repository's subtree when they lie inside it; elsewhere in the
		// checkout they are bound on their own. Either way they are read strictly, since symlinks or
		// alternates inside them would hide Git state that a discard drops. Outside the checkout, or
		// not resolving, a discard could not show what it drops.
		var inside, elsewhere []string
		if _, err := root.Lstat(path.Join(repo, ".git")); err == nil {
			if layout, err := gitexclude.Resolve(filepath.Join(st.root, filepath.FromSlash(repo))); err != nil {
				d.incomplete("resolve nested .git", err)
			} else {
				for _, dir := range []string{layout.GitDir(), layout.CommonDir()} {
					real, err := filepath.EvalSymlinks(dir)
					if err != nil {
						d.incomplete("resolve nested Git directory", err)
						continue
					}
					rel, err := filepath.Rel(realRoot, real)
					if err != nil || !filepath.IsLocal(rel) || rel == "." {
						d.incomplete("nested Git directory outside the checkout", nil)
						continue
					}
					rel = filepath.ToSlash(rel)
					if within(rel, []string{repo}) {
						inside = append(inside, rel)
					} else {
						elsewhere = append(elsewhere, rel)
					}
				}
			}
		}
		d.repoTree(root, repo, inside)
		digested = append(digested, repo)
		checkedGitDirs = append(checkedGitDirs, inside...)
		for _, rel := range elsewhere {
			if !within(rel, digested) {
				fmt.Fprintf(h, "points at %s\x00", rel)
				d.gitTree(root, rel)
				digested = append(digested, rel)
				checkedGitDirs = append(checkedGitDirs, rel)
				continue
			}
			if !within(rel, checkedGitDirs) {
				d.checkGitDir(root, rel)
				checkedGitDirs = append(checkedGitDirs, rel)
				continue
			}
			d.rejectAlternates(root, rel)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	return hex.EncodeToString(h.Sum(nil)), d.complete, nil
}

// within reports that rel is one of dirs or below one.
func within(rel string, dirs []string) bool {
	for _, d := range dirs {
		if rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

// digester writes the bytes of files into h within a budget of content bytes and entries. Once
// anything cannot be bound, complete is false for good.
type digester struct {
	ctx      context.Context
	h        io.Writer
	bytes    int64
	entries  int
	complete bool
}

func (d *digester) incomplete(what string, err error) {
	d.complete = false
	fmt.Fprintf(d.h, "incomplete %s: %v\x00", what, err)
}

// file writes the content of name, which must be a regular file within the remaining budget; a
// missing file writes a marker. It never follows a link and never blocks on a special file.
func (d *digester) file(r *os.Root, name string) {
	info, err := r.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(d.h, "%s -\x00", name)
		return
	}
	if err != nil {
		d.incomplete(name, err)
		return
	}
	if !info.Mode().IsRegular() {
		d.incomplete(name+" is not a regular file", nil)
		return
	}
	if info.Size() > d.bytes {
		d.incomplete(name+" exceeds the digest limit", nil)
		return
	}
	f, err := r.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		d.incomplete(name, err)
		return
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		d.incomplete(name+" changed while it was read", err)
		return
	}
	fmt.Fprintf(d.h, "file %s %d\x00", name, opened.Size())
	n, err := io.Copy(d.h, io.LimitReader(f, d.bytes+1))
	if err != nil {
		d.incomplete(name, err)
		return
	}
	if n > d.bytes {
		d.incomplete(name+" grew past the digest limit", nil)
		return
	}
	d.bytes -= n
}

// repoTree writes a nested repository: its working tree by content and links by target, and the
// Git directories inside it (gitDirs, relative to the checkout root) strictly, all in one pass.
// One walk binds the whole repository, so the entry budget counts each entry once.
func (d *digester) repoTree(r *os.Root, dir string, gitDirs []string) {
	d.walkTree(r, dir, gitDirs, true)
}

// gitTree writes every entry below a nested Git directory by content and fails closed when any
// symlink, special file, or objects/info/alternates is encountered inside it.
func (d *digester) gitTree(r *os.Root, dir string) {
	dir = path.Clean(dir)
	info, err := r.Lstat(dir)
	if err != nil {
		d.incomplete(dir, err)
		return
	}
	if !info.IsDir() {
		d.incomplete(dir+" is not a directory", nil)
		return
	}
	d.walkTree(r, dir, []string{dir}, false)
}

// walkTree writes every entry below dir. Inside the directories in strict (which include dir
// itself when it is a Git directory) a delivery cannot carry what Git reads there, so a symlink or
// special file, and an objects/info/alternates at any depth, make the digest incomplete instead of
// being followed. allowMissing writes a marker for a gitlink that has no working tree here.
func (d *digester) walkTree(r *os.Root, dir string, strict []string, allowMissing bool) {
	if _, err := r.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		if allowMissing {
			fmt.Fprintf(d.h, "missing %s\x00", dir)
			return // a gitlink with no working tree here
		}
		d.incomplete(dir, err)
		return
	}
	err := fs.WalkDir(r.FS(), dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := d.ctx.Err(); err != nil {
			return err
		}
		if d.entries++; d.entries > maxNestedEntries {
			d.incomplete("too many entries", nil)
			return fs.SkipAll
		}
		inGit := strict != nil && within(p, strict)
		if inGit && strings.HasSuffix(p, "/objects/info/alternates") {
			d.incomplete(p+" uses alternates in a nested Git directory", nil)
			return fs.SkipAll
		}
		switch t := e.Type(); {
		case t.IsDir():
			fmt.Fprintf(d.h, "dir %s\x00", p)
		case t&fs.ModeSymlink != 0:
			if inGit {
				d.incomplete(p+" is a symlink in a nested Git directory", nil)
				return fs.SkipAll
			}
			target, err := r.Readlink(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(d.h, "link %s %s\x00", p, target)
		case t.IsRegular():
			d.file(r, p)
		default:
			d.incomplete(p+" is a special file", nil)
		}
		if !d.complete {
			return fs.SkipAll // nothing past this can make the digest complete again
		}
		return nil
	})
	if err != nil {
		d.incomplete(dir, err)
	}
}

// checkGitDir verifies that an already-digested nested Git directory (for example a .git
// subdirectory walked via tree(root, repo)) contains no symlinks, special files, or
// objects/info/alternates.
func (d *digester) checkGitDir(r *os.Root, dir string) {
	if !d.complete {
		return
	}
	dir = path.Clean(dir)
	info, err := r.Lstat(dir)
	if err != nil {
		d.incomplete(dir, err)
		return
	}
	if !info.IsDir() {
		d.incomplete(dir+" is not a directory", nil)
		return
	}
	// A Git directory's alternates may sit at any depth (a deinitialized submodule keeps its own).
	const alternatesSuffix = "/objects/info/alternates"
	checked := 0
	err = fs.WalkDir(r.FS(), dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := d.ctx.Err(); err != nil {
			return err
		}
		if checked++; checked > maxNestedEntries {
			d.incomplete("too many entries", nil)
			return fs.SkipAll
		}
		if strings.HasSuffix(p, alternatesSuffix) {
			d.incomplete(p+" uses alternates in a nested Git directory", nil)
			return fs.SkipAll
		}
		t := e.Type()
		switch {
		case t&fs.ModeSymlink != 0:
			d.incomplete(p+" is a symlink in a nested Git directory", nil)
			return fs.SkipAll
		case !t.IsDir() && !t.IsRegular():
			d.incomplete(p+" is a special file", nil)
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		d.incomplete(dir, err)
	}
}

func (d *digester) rejectAlternates(r *os.Root, dir string) {
	p := path.Join(path.Clean(dir), "objects/info/alternates")
	if _, err := r.Lstat(p); err == nil || !errors.Is(err, fs.ErrNotExist) {
		d.incomplete(p+" uses alternates in a nested Git directory", err)
	}
}

// maxTreeListing bounds a listing of the HEAD tree, the index or the delivery's tree.
const maxTreeListing = 64 << 20

// headAndIndex compares what HEAD and the index hold with the delivery's tree target, since
// neither is in the snapshot (which reads the working tree). HEAD is fine when it is the base or
// one of its ancestors, or when every path its tree tracks has the same mode and object in the
// target or in the base, whose snapshot the mirror keeps (untracked files the delivery added do
// not matter; content only an intermediate commit
// held does not count, as for refs). The index is fine when it has no conflict and every entry
// has the same mode and object as the target or the base at its path, or as HEAD (an unstaged change: HEAD's
// content is the base's or the delivery's when HEAD is fine); an intent-to-add entry (the empty
// blob) only needs the path in the target, since its content is the working tree's. Paths under Ora's
// materialized skill roots that the delivery left out are skipped, as the delivery strips them.
func (s *DeliveryFS) headAndIndex(ctx context.Context, agent deliveryGit, st *staged, target string) (headDiffers bool, headCommits int, indexDiffers bool, err error) {
	listing := func(g deliveryGit, args ...string) ([]byte, error) {
		raw, err := g.run(ctx, nil, maxTreeListing, args...)
		if errors.Is(err, ErrTooLarge) {
			return nil, fmt.Errorf("%w: %w", ErrUnverifiable, err)
		}
		return raw, err
	}
	raw, err := listing(st.git, "ls-tree", "-r", "-z", "--full-tree", target)
	if err != nil {
		return false, 0, false, err
	}
	type entry struct{ mode, oid string }
	delivered := map[string]entry{}
	for _, line := range strings.Split(string(raw), "\x00") {
		meta, path, ok := strings.Cut(line, "\t")
		if f := strings.Fields(meta); ok && len(f) == 3 {
			delivered[path] = entry{f[0], f[2]}
		}
	}
	baseRaw, err := listing(st.git, "ls-tree", "-r", "-z", "--full-tree", st.baseTree)
	if err != nil {
		return false, 0, false, err
	}
	based := map[string]entry{}
	for _, line := range strings.Split(string(baseRaw), "\x00") {
		meta, path, ok := strings.Cut(line, "\t")
		if f := strings.Fields(meta); ok && len(f) == 3 {
			based[path] = entry{f[0], f[2]}
		}
	}
	stripped := func(path string) bool {
		if _, ok := delivered[path]; ok {
			return false
		}
		for _, root := range materializedRoots {
			if strings.HasPrefix(path, root+"/") {
				return true
			}
		}
		return false
	}
	headEntries := map[string]entry{}
	present, err := s.headPresent(ctx, agent, st)
	if err != nil {
		return false, 0, false, err
	}
	if present {
		head, err := listing(agent, "ls-tree", "-r", "-z", "--full-tree", "HEAD")
		if err != nil {
			return false, 0, false, corrupt(err)
		}
		for _, line := range strings.Split(string(head), "\x00") {
			meta, path, ok := strings.Cut(line, "\t")
			if f := strings.Fields(meta); ok && len(f) == 3 {
				headEntries[path] = entry{f[0], f[2]}
			}
		}
		if _, err := agent.value(ctx, "merge-base", "--is-ancestor", "HEAD", st.base); err != nil {
			for path, e := range headEntries {
				if !stripped(path) && delivered[path] != e && based[path] != e {
					headDiffers = true
					break
				}
			}
			if headDiffers {
				if n, err := agent.value(ctx, "rev-list", "--count", "HEAD", "--not", st.base, "--"); err == nil {
					headCommits, _ = strconv.Atoi(n)
				}
			}
		}
	}
	index, err := listing(agent, "ls-files", "--stage", "-z")
	if err != nil {
		return false, 0, false, err
	}
	empty, err := agent.value(ctx, "hash-object", "-t", "blob", "/dev/null")
	if err != nil {
		return false, 0, false, err
	}
	for _, line := range strings.Split(string(index), "\x00") {
		meta, path, ok := strings.Cut(line, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			continue
		}
		if f[2] != "0" {
			indexDiffers = true // a conflict stage
			break
		}
		if stripped(path) {
			continue
		}
		staged := entry{f[0], f[1]}
		got, inTarget := delivered[path]
		switch {
		case staged == got, staged == based[path]:
		case f[1] == empty && inTarget:
			// intent-to-add, or an empty staged file whose path the delivery has
		case !headDiffers && headEntries[path] == staged:
			// unstaged: the entry is HEAD's, whose content the base or the delivery holds
		default:
			indexDiffers = true
		}
		if indexDiffers {
			break
		}
	}
	return headDiffers, headCommits, indexDiffers, nil
}

// corrupt marks a Git failure that Git attributes to the repository's content (an object it names
// is missing, malformed or not what it should be) as ErrUnverifiable, which retrying cannot
// change. Anything else (I/O, permissions, a failed start, a timeout, a cancellation) stays as it
// is and is retried.
func corrupt(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	msg := err.Error()
	// An I/O or permission diagnostic wins over the generic object complaint Git adds after it.
	if ioTrouble(msg) {
		return err
	}
	for _, sign := range []string{"missing object", "bad object", "not a tree object", "not a valid object", "invalid object", "corrupt",
		"unterminated line in", "unexpected line in"} {
		if strings.Contains(msg, sign) {
			return fmt.Errorf("%w: %w", ErrUnverifiable, err)
		}
	}
	return err
}

// ioTrouble reports an I/O or permission diagnostic in Git's output: such a failure may pass.
func ioTrouble(msg string) bool {
	for _, sign := range []string{"Permission denied", "Input/output error", "unable to open loose object", "EACCES", "EIO",
		"No space left", "Too many open files", "Stale file handle", "Resource temporarily unavailable"} {
		if strings.Contains(msg, sign) {
			return true
		}
	}
	return false
}

// objectDamage reports a diagnostic in which Git says an object file itself is damaged.
func objectDamage(msg string) bool {
	for _, sign := range []string{"corrupt loose object", "is corrupt", "inflate", "is empty", "unable to unpack", "bad object", "hash mismatch"} {
		if strings.Contains(msg, sign) {
			return true
		}
	}
	return false
}

// headPresent tells whether HEAD names a commit to compare, failing closed: only a symbolic HEAD
// naming a ref that does not exist (an unborn branch) has no commit. A detached HEAD, or one
// naming an existing ref, that does not resolve to a readable commit makes the checkout
// unverifiable; it is never taken for a missing HEAD.
func (s *DeliveryFS) headPresent(ctx context.Context, agent deliveryGit, st *staged) (bool, error) {
	_, resolveErr := agent.value(ctx, "rev-parse", "-q", "--verify", "HEAD^{commit}")
	if resolveErr == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	// HEAD's own file says what it is (read without Git, so no newer Git option is needed): a
	// symbolic ref names its direct target, otherwise it holds an object ID. Only the direct target
	// matters: a HEAD naming an existing alias that dangles is not unborn.
	layout, err := gitexclude.Resolve(st.root)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrUnverifiable, err)
	}
	gitdir, err := os.OpenRoot(layout.GitDir())
	if err != nil {
		return false, err
	}
	raw, err := readSmall(gitdir, "HEAD")
	gitdir.Close()
	if err != nil {
		return false, err
	}
	head := strings.TrimSpace(raw)
	var oid string
	if ref, ok := strings.CutPrefix(head, "ref: "); ok {
		if !validRefName(ref) {
			return false, fmt.Errorf("%w: HEAD names %q", ErrUnverifiable, ref)
		}
		value, recorded, err := refValue(st.root, ref, 0)
		if err != nil {
			return false, err
		}
		if !recorded {
			return false, nil // unborn: nothing committed yet
		}
		oid = value
	} else {
		oid = head
	}
	if !validCommit(oid) {
		return false, fmt.Errorf("%w: HEAD does not name an object", ErrUnverifiable)
	}
	// Why HEAD did not resolve decides, from every diagnostic of rev-parse and cat-file -t, in this
	// order: I/O or permission trouble may pass (retried with the original error); an object file
	// Git calls damaged, an object reported absent, or one that is not a commit is a lasting state
	// of the checkout (unverifiable); anything else, silence included, is retried.
	kind, err := agent.value(ctx, "cat-file", "-t", oid)
	said := resolveErr.Error()
	if err != nil {
		said += "\n" + err.Error()
	}
	switch {
	case ioTrouble(said):
		return false, resolveErr
	case objectDamage(said):
		// cat-file -t may still read the type from a half-written object; rev-parse reading it
		// whole is what reports the damage.
		return false, fmt.Errorf("%w: HEAD's commit %s is damaged: %w", ErrUnverifiable, oid, resolveErr)
	case err == nil && kind != "commit":
		return false, fmt.Errorf("%w: HEAD names a %s, not a commit", ErrUnverifiable, kind)
	case err != nil && strings.Contains(err.Error(), "could not get object info") && objectAbsent(layout, oid):
		// Older Git says the same of an object it may not read, so the absence is confirmed on disk.
		return false, fmt.Errorf("%w: HEAD's commit %s is missing", ErrUnverifiable, oid)
	default:
		return false, resolveErr
	}
}

// objectAbsent confirms on disk that the repository has no copy of oid it merely could not read:
// the loose object's directory is readable and holds no such file, every pack and index can be
// opened, and there are no alternates to look in. Anything that cannot be confirmed counts as not
// absent, so the caller retries.
func objectAbsent(layout gitexclude.Layout, oid string) bool {
	objects, err := os.OpenRoot(filepath.Join(layout.CommonDir(), "objects"))
	if err != nil {
		return false
	}
	defer objects.Close()
	if _, err := objects.Lstat("info/alternates"); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if _, err := objects.Lstat(oid[:2]); err == nil {
		if _, err := fs.ReadDir(objects.FS(), oid[:2]); err != nil {
			return false
		}
		if _, err := objects.Lstat(oid[:2] + "/" + oid[2:]); !errors.Is(err, fs.ErrNotExist) {
			return false
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	packs, err := fs.ReadDir(objects.FS(), "pack")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	for _, p := range packs {
		f, err := objects.OpenFile("pack/"+p.Name(), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return false
		}
		f.Close()
	}
	return true
}

// refValue reads what the repository records for ref (a loose file, else its packed-refs line),
// following symbolic refs: the object ID it ends at, and whether the ref is recorded at all. A
// symbolic ref whose target is not recorded dangles and is unverifiable.
func refValue(root, ref string, depth int) (string, bool, error) {
	if depth > 5 {
		return "", false, fmt.Errorf("%w: symbolic refs nest too deep at %s", ErrUnverifiable, ref)
	}
	if !strings.HasPrefix(ref, "refs/") || !filepath.IsLocal(ref) {
		return "", false, fmt.Errorf("%w: HEAD names %q", ErrUnverifiable, ref)
	}
	layout, err := gitexclude.Resolve(root)
	if err != nil {
		return "", false, fmt.Errorf("%w: %w", ErrUnverifiable, err)
	}
	common, err := os.OpenRoot(layout.CommonDir())
	if err != nil {
		return "", false, err
	}
	defer common.Close()
	if _, err := common.Lstat("reftable"); err == nil {
		return "", false, fmt.Errorf("%w: the repository uses a reftable", ErrUnverifiable)
	}
	value := ""
	if raw, err := readSmall(common, ref); err == nil {
		value = strings.TrimSpace(raw)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	} else {
		packed, err := readSmall(common, "packed-refs")
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", false, err
		}
		for _, line := range strings.Split(packed, "\n") {
			if oid, name, ok := strings.Cut(line, " "); ok && strings.TrimSpace(name) == ref && !strings.HasPrefix(line, "#") {
				value = oid
				break
			}
		}
		if value == "" {
			return "", false, nil
		}
	}
	if target, ok := strings.CutPrefix(value, "ref: "); ok {
		next, recorded, err := refValue(root, target, depth+1)
		if err != nil {
			return "", false, err
		}
		if !recorded {
			return "", false, fmt.Errorf("%w: %s names %s, which is not recorded", ErrUnverifiable, ref, target)
		}
		return next, true, nil
	}
	return value, true, nil
}

// checkRecordedRefs reads the refs the repository records as files (loose refs under refs/ of
// its common and its own Git directory, and packed-refs) without following links or blocking, and
// requires each to be a well-formed object ID or symbolic ref, and each object ID ref to be one
// for-each-ref listed (when listed is not nil): a ref Git could not read is not taken for one that
// is absent. A reftable
// cannot be read this way and counts as unverifiable.
func checkRecordedRefs(layout gitexclude.Layout, listed map[string]bool) error {
	dirs := []string{layout.CommonDir()}
	if layout.GitDir() != layout.CommonDir() {
		dirs = append(dirs, layout.GitDir())
	}
	for _, dir := range dirs {
		r, err := os.OpenRoot(dir)
		if err != nil {
			return err
		}
		err = func() error {
			defer r.Close()
			if _, err := r.Lstat("reftable"); err == nil {
				return fmt.Errorf("%w: the repository uses a reftable", ErrUnverifiable)
			}
			check := func(name, value string) error {
				value = strings.TrimSpace(value)
				if target, ok := strings.CutPrefix(value, "ref: "); ok {
					if !strings.HasPrefix(target, "refs/") {
						return fmt.Errorf("%w: ref %s names %q", ErrUnverifiable, name, target)
					}
					return nil // a symbolic ref holds no object of its own
				}
				if !validCommit(value) {
					return fmt.Errorf("%w: ref %s is malformed", ErrUnverifiable, name)
				}
				if listed != nil && !listed[name] {
					return fmt.Errorf("%w: ref %s cannot be read", ErrUnverifiable, name)
				}
				return nil
			}
			err := fs.WalkDir(r.FS(), "refs", func(p string, e fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if e.IsDir() {
					return nil
				}
				if !validRefName(p) {
					return nil // not a ref to Git (a stale .lock, say)
				}
				content, err := readSmall(r, p)
				if err != nil {
					return err
				}
				return check(p, content)
			})
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			packed, err := readSmall(r, "packed-refs")
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			// The whole structure, as Git would read it: lines end with a newline, an optional
			// header comment comes first, then "<oid> <ref>" lines, each optionally followed by one
			// "^<oid>" peeled line.
			if packed != "" && !strings.HasSuffix(packed, "\n") {
				return fmt.Errorf("%w: packed-refs ends in an unterminated line", ErrUnverifiable)
			}
			afterRef := false
			for i, line := range strings.Split(strings.TrimSuffix(packed, "\n"), "\n") {
				switch {
				case packed == "":
				case i == 0 && strings.HasPrefix(line, "# pack-refs with:"):
					afterRef = false
				case strings.HasPrefix(line, "^"):
					if !afterRef || !validCommit(line[1:]) {
						return fmt.Errorf("%w: packed-refs has a malformed peeled line", ErrUnverifiable)
					}
					afterRef = false
				default:
					oid, name, ok := strings.Cut(line, " ")
					if !ok || !validCommit(oid) || !strings.HasPrefix(name, "refs/") || strings.ContainsAny(name, " \t") {
						return fmt.Errorf("%w: packed-refs is malformed", ErrUnverifiable)
					}
					afterRef = true
					// A packed ref a loose one overrides is not what Git uses.
					if _, err := r.Lstat(name); err == nil {
						continue
					}
					if err := check(name, oid); err != nil {
						return err
					}
				}
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

// validRefName reports whether Git would take name as a ref (git check-ref-format's rules): no
// component empty, starting with a dot or ending in ".lock", no "..", no "@{", no control
// characters, space or any of ~ ^ : ? * [ \, not "@", not ending in "/" or ".". A file under
// refs/ with another name (a stale lock left by an interrupted Git) is not a ref.
func validRefName(name string) bool {
	if name == "@" || name == "" || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") ||
		strings.Contains(name, "..") || strings.Contains(name, "@{") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// readSmall reads a regular file of Git metadata within maxTreeListing, without following a link
// or blocking on a special file; anything else is unverifiable.
func readSmall(r *os.Root, name string) (string, error) {
	info, err := r.Lstat(name)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maxTreeListing {
		return "", fmt.Errorf("%w: %s is not a regular file within limits", ErrUnverifiable, name)
	}
	f, err := r.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxTreeListing))
	return string(raw), err
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
