package workflowhost

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// Deliverable checks of agentConfig.completion.files (desktop 0fcc3b30 interactive/
// completion_files.rs; specs 20261023-interactive-deliverable-contract.md). They run on every
// completion against the run's checkout, which v2-server shares the host with, and are never
// written into the park snapshot: the checkout is mutable, and a starter who adds a missing file
// expects the next completion to pass.
//
// Files are enumerated as desktop does: literal declarations are looked up directly, wildcards
// through git's tracked and untracked, not ignored files under the declaration's literal prefix
// (so node_modules and other ignored trees are never walked). Every git call here reads only:
// no filter, textconv, fsmonitor or hook the Agent may have configured in the checkout runs.
//
// mustChange compares against the node's baseline, recorded once when its first turn is
// dispatched and written once into the node's payload (completion_baseline) so it survives a
// restart. Desktop snapshots the whole worktree; v2 records one digest per mustChange declaration
// over its matches in the run's checkout, or, when no Agent of the run has a checkout yet, the
// commit the checkout will be cloned at, which is then the files' state before anything ran.
//
// A check the bounds cut short (too many listed entries, or more matches than are compared) is
// never a blocker: the declaration is reported as file_check_unavailable instead.

const (
	// baselineCheckout: one digest per mustChange declaration, taken as the node started.
	baselineCheckout = "checkout"
	// baselineCommit: the run's checkout did not exist yet; compare with its base commit.
	baselineCommit = "base_commit"
	// baselineMissing: the baseline could not be recorded; only existence is judged and the
	// snapshot carries file_check_unavailable.
	baselineMissing = "unavailable"

	gitTimeout = 10 * time.Second
)

// The bounds of one check; tests lower them.
var (
	// maxListedEntries bounds the files one listing reads; past it, wildcard declarations
	// cannot be judged.
	maxListedEntries = 50000
	// maxComparedFiles bounds the matches one mustChange declaration digests or compares.
	maxComparedFiles = 200
)

// fileBaseline is payload.completion_baseline.
type fileBaseline struct {
	Source string `json:"source"`
	// Declarations maps each mustChange declaration's path to the digest of its matches (source
	// checkout); "" when they could not be recorded within the bounds.
	Declarations map[string]string `json:"declarations,omitempty"`
}

func (b *fileBaseline) unavailable() bool { return b.Source == baselineMissing }

// needsBaseline reports whether any declaration asks for a change.
func needsBaseline(files []workflow.CompletionFile) bool {
	for _, f := range files {
		if f.MustChange {
			return true
		}
	}
	return false
}

// captureBaseline records the baseline of files for a node of runID about to start its first turn.
func (h *Host) captureBaseline(ctx context.Context, tenant, runID string, files []workflow.CompletionFile) *fileBaseline {
	if !needsBaseline(files) {
		return nil
	}
	at, err := h.checkoutOf(ctx, tenant, runID)
	if errors.Is(err, store.ErrNotFound) {
		return &fileBaseline{Source: baselineCommit}
	}
	if err != nil {
		h.log.Warn("read the checkout for a completion baseline", "run", runID, "error", err)
		return &fileBaseline{Source: baselineMissing}
	}
	l := listFiles(ctx, at.Path, files)
	b := &fileBaseline{Source: baselineCheckout, Declarations: map[string]string{}}
	for _, f := range files {
		if !f.MustChange {
			continue
		}
		digest := ""
		if matches, judged := l.matches(f); judged {
			digest, _ = matchesDigest(at.Path, matches)
		}
		b.Declarations[f.Path] = digest
	}
	return b
}

// fileBlockers checks n's declarations in the run's checkout: file_missing for a declaration no
// regular file matches, file_unchanged for a mustChange declaration none of whose matches differs
// from the baseline. warnings carries file_check_unavailable when a declaration could not be
// judged.
func (h *Host) fileBlockers(ctx context.Context, tenant, runID string, files []workflow.CompletionFile, baseline *fileBaseline) (blockers []completionBlocker, warnings []string) {
	if len(files) == 0 {
		return nil, nil
	}
	at, err := h.checkoutOf(ctx, tenant, runID)
	if err != nil {
		// No checkout at all: nothing the declarations name can exist.
		for _, f := range files {
			blockers = append(blockers, fileBlocker(blockerFileMissing, f.Path))
		}
		return blockers, nil
	}
	l := listFiles(ctx, at.Path, files)
	for _, f := range files {
		matches, judged := l.matches(f)
		if len(matches) == 0 {
			if judged {
				blockers = append(blockers, fileBlocker(blockerFileMissing, f.Path))
			} else {
				warnings = append(warnings, warningFileCheck)
			}
			continue
		}
		if !f.MustChange {
			continue
		}
		changed, sure := changedSince(ctx, at, f, matches, judged, baseline)
		switch {
		case !sure:
			warnings = append(warnings, warningFileCheck)
		case !changed:
			blockers = append(blockers, fileBlocker(blockerFileUnchanged, f.Path))
		}
	}
	return blockers, dedupe(warnings)
}

func fileBlocker(kind, path string) completionBlocker {
	return completionBlocker{Kind: kind, Params: map[string]any{"path": path}}
}

// changedSince reports whether declaration f's matches differ from the baseline; sure is false
// when that cannot be told within the bounds. complete says matches is every match of f.
func changedSince(ctx context.Context, at store.WorkspaceLocation, f workflow.CompletionFile, matches []string, complete bool, baseline *fileBaseline) (changed, sure bool) {
	if baseline == nil {
		return false, false
	}
	switch baseline.Source {
	case baselineCheckout:
		before := baseline.Declarations[f.Path]
		if before == "" || !complete {
			return false, false
		}
		after, ok := matchesDigest(at.Path, matches)
		if !ok {
			return false, false
		}
		return after != before, true
	case baselineCommit:
		// Any match that differs from the commit is a change, even in a partial listing.
		if len(matches) > maxComparedFiles {
			matches = matches[:maxComparedFiles]
			complete = false
		}
		for _, path := range matches {
			before, err := committedDigest(ctx, at.Path, at.BaseCommit, path)
			if err != nil {
				return false, false
			}
			after, err := fileDigest(at.Path, path)
			if err != nil {
				continue // gone since the listing
			}
			if before != after {
				return true, true
			}
		}
		return false, complete
	default:
		return false, false
	}
}

// matchesDigest digests a declaration's matches, paths and contents, so a change to any of them,
// an added or a removed match, changes it. ok is false past maxComparedFiles or when a match
// cannot be read.
func matchesDigest(root string, matches []string) (string, bool) {
	if len(matches) > maxComparedFiles {
		return "", false
	}
	sorted := append([]string(nil), matches...)
	sort.Strings(sorted)
	h := sha256.New()
	for _, path := range sorted {
		digest, err := fileDigest(root, path)
		if err != nil {
			return "", false
		}
		fmt.Fprintf(h, "%s\x00%s\n", path, digest)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), true
}

// listing is the regular files of a checkout the declarations could match.
type listing struct {
	found map[string]bool
	// complete is false when wildcard declarations were enumerated only in part.
	complete bool
}

// matches returns f's matches, sorted; judged is false when f is a wildcard and the listing is
// partial.
func (l listing) matches(f workflow.CompletionFile) (matches []string, judged bool) {
	if f.Literal() {
		if l.found[f.Path] {
			return []string{f.Path}, true
		}
		return nil, true
	}
	for path := range l.found {
		if f.Matches(path) {
			matches = append(matches, path)
		}
	}
	sort.Strings(matches)
	return matches, l.complete
}

// listFiles lists the regular files under root that any declaration matches, by slash-separated
// relative path. Symbolic links never count (a link could name a file outside the checkout), nor
// does anything under .git.
func listFiles(ctx context.Context, root string, files []workflow.CompletionFile) listing {
	l := listing{found: map[string]bool{}, complete: true}
	var wildcards []workflow.CompletionFile
	for _, f := range files {
		if !f.Literal() {
			wildcards = append(wildcards, f)
			continue
		}
		// Directly, like desktop's targeted capture: a declared file counts even where git
		// ignores it.
		if regularFile(root, f.Path) {
			l.found[f.Path] = true
		}
	}
	if len(wildcards) == 0 {
		return l
	}
	add := func(rel string) {
		for _, f := range wildcards {
			if f.Matches(rel) {
				if regularFile(root, rel) {
					l.found[rel] = true
				}
				return
			}
		}
	}
	prefixes := wildcardPrefixes(wildcards)
	complete, err := gitListFiles(ctx, root, prefixes, add)
	if err != nil {
		// Not a git checkout: walk the prefixes instead, without gitignore.
		complete = walkFiles(root, prefixes, add)
	}
	l.complete = complete
	return l
}

// wildcardPrefixes are the directories the wildcard declarations' matches lie under, without
// one nested in another; "" alone stands for the whole checkout.
func wildcardPrefixes(wildcards []workflow.CompletionFile) []string {
	var all []string
	for _, f := range wildcards {
		all = append(all, f.Prefix())
	}
	sort.Strings(all)
	var prefixes []string
	for _, p := range all {
		if p == "" {
			return []string{""}
		}
		if n := len(prefixes); n > 0 && (p == prefixes[n-1] || strings.HasPrefix(p, prefixes[n-1]+"/")) {
			continue
		}
		prefixes = append(prefixes, p)
	}
	return prefixes
}

// gitListFiles feeds add git's tracked and untracked, not ignored paths under prefixes, up to
// maxListedEntries; complete is false when the bound cut the listing short.
func gitListFiles(ctx context.Context, root string, prefixes []string, add func(rel string)) (complete bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	args := []string{"ls-files", "-z", "--cached", "--others", "--exclude-standard", "--"}
	for _, p := range prefixes {
		if p == "" {
			p = "."
		}
		args = append(args, p)
	}
	cmd := gitCommand(ctx, root, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024)
	scanner.Split(splitNUL)
	entries, seen := 0, map[string]bool{}
	complete = true
	for scanner.Scan() {
		rel := scanner.Text()
		if seen[rel] {
			continue // a file both tracked and modified is listed once per stage
		}
		seen[rel] = true
		if entries++; entries > maxListedEntries {
			complete = false
			cancel()
			break
		}
		add(rel)
	}
	_, _ = io.Copy(io.Discard, out)
	waitErr := cmd.Wait()
	if !complete {
		return false, nil
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return true, waitErr
}

func splitNUL(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// walkFiles walks prefixes under root, skipping .git, and feeds add every regular file, up to
// maxListedEntries; it reports whether the walk finished.
func walkFiles(root string, prefixes []string, add func(rel string)) bool {
	entries := 0
	for _, prefix := range prefixes {
		start := filepath.Join(root, filepath.FromSlash(prefix))
		if prefix != "" && !realDir(root, prefix) {
			continue
		}
		stopped := false
		_ = filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // an unreadable entry matches nothing
			}
			if entries++; entries > maxListedEntries {
				stopped = true
				return filepath.SkipAll
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			if rel, err := filepath.Rel(root, path); err == nil {
				add(filepath.ToSlash(rel))
			}
			return nil
		})
		if stopped {
			return false
		}
	}
	return true
}

// realDir reports whether rel names a directory inside root, no component of which is a
// symbolic link or .git.
func realDir(root, rel string) bool {
	current := root
	for _, part := range strings.Split(rel, "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || part == ".git" {
			return false
		}
	}
	return true
}

// regularFile reports whether rel names a regular file inside root, no component of which is a
// symbolic link.
func regularFile(root, rel string) bool {
	parts := strings.Split(rel, "/")
	if len(parts) > 1 && !realDir(root, strings.Join(parts[:len(parts)-1], "/")) {
		return false
	}
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil && info.Mode().IsRegular()
}

func fileDigest(root, rel string) (string, error) {
	if !regularFile(root, rel) {
		return "", fs.ErrNotExist
	}
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// committedDigest is fileDigest of rel's content at commit, "" when the commit does not have it.
// The blob is read as stored, so no clean filter of the checkout's .gitattributes runs, and both
// sides are hashed here.
func committedDigest(ctx context.Context, root, commit, rel string) (string, error) {
	if commit == "" {
		return "", errors.New("no base commit recorded")
	}
	if _, err := git(ctx, root, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return "", err
	}
	if _, err := git(ctx, root, "cat-file", "-e", commit+":"+rel); err != nil {
		return "", nil // the path is not in the commit
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	h := sha256.New()
	cmd := gitCommand(ctx, root, "cat-file", "blob", commit+":"+rel)
	cmd.Stdout = h
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func git(ctx context.Context, root string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := gitCommand(ctx, root, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// gitCommand is a read-only git call in the checkout, whose configuration the Agent controls: as
// workspacefs.runGit, nothing it configured may run a program (fsmonitor, hooks), paths are taken
// literally, and git takes no optional lock. Callers only list, test and print objects as stored,
// so no filter or textconv applies either.
func gitCommand(ctx context.Context, root string, args ...string) *exec.Cmd {
	base := []string{"-C", root, "--literal-pathspecs", "-c", "core.quotepath=off", "-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null", "-c", "core.untrackedCache=false"}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	return cmd
}

func dedupe(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
