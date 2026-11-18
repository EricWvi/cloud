package workflowhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
// mustChange compares against the node's baseline. Desktop snapshots the whole worktree when the
// node starts; v2 records only the declared files: their digests in the run's checkout when the
// node's first turn is dispatched, or, when no Agent of the run has a checkout yet, the commit the
// checkout will be cloned at, which is then the files' state before anything ran. The baseline
// lives in the node's payload (completion_baseline), so it survives a restart.

const (
	// baselineCheckout: the declared files' digests as the node started.
	baselineCheckout = "checkout"
	// baselineCommit: the run's checkout did not exist yet; compare with its base commit.
	baselineCommit = "base_commit"
	// baselineMissing: the baseline could not be recorded; only existence is judged and the
	// snapshot carries file_check_unavailable.
	baselineMissing = "unavailable"

	// maxWalkedEntries bounds one walk of the checkout; past it, wildcard declarations are judged
	// on the entries seen so far and the check warns.
	maxWalkedEntries = 50000
	// maxComparedFiles bounds the files one mustChange declaration compares.
	maxComparedFiles = 200
	gitTimeout       = 10 * time.Second
)

// fileBaseline is payload.completion_baseline.
type fileBaseline struct {
	Source string `json:"source"`
	// Files maps every declared file that existed to its digest (source checkout).
	Files map[string]string `json:"files,omitempty"`
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
	found, _ := matchingFiles(at.Path, files)
	b := &fileBaseline{Source: baselineCheckout, Files: map[string]string{}}
	for path := range found {
		if digest, err := fileDigest(at.Path, path); err == nil {
			b.Files[path] = digest
		}
	}
	return b
}

// fileBlockers checks n's declarations in the run's checkout: file_missing for a declaration no
// regular file matches, file_unchanged for a mustChange declaration none of whose matches differs
// from the baseline. warnings carries file_check_unavailable when a change could not be judged.
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
	found, truncated := matchingFiles(at.Path, files)
	if truncated {
		warnings = append(warnings, warningFileCheck)
	}
	for _, f := range files {
		var matches []string
		for path := range found {
			if f.Matches(path) {
				matches = append(matches, path)
			}
		}
		if len(matches) == 0 {
			blockers = append(blockers, fileBlocker(blockerFileMissing, f.Path))
			continue
		}
		if !f.MustChange {
			continue
		}
		changed, judged := changedSince(ctx, at, matches, baseline)
		switch {
		case !judged:
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

// changedSince reports whether any of matches differs from the baseline; judged is false when
// the baseline cannot tell.
func changedSince(ctx context.Context, at store.WorkspaceLocation, matches []string, baseline *fileBaseline) (changed, judged bool) {
	if baseline == nil || baseline.unavailable() {
		return false, false
	}
	sort.Strings(matches)
	if len(matches) > maxComparedFiles {
		matches = matches[:maxComparedFiles]
	}
	for _, path := range matches {
		switch baseline.Source {
		case baselineCheckout:
			before, existed := baseline.Files[path]
			after, err := fileDigest(at.Path, path)
			if err != nil {
				continue
			}
			if !existed || before != after {
				return true, true
			}
		case baselineCommit:
			before, err := committedBlob(ctx, at.Path, at.BaseCommit, path)
			if err != nil {
				return false, false
			}
			after, err := git(ctx, at.Path, "hash-object", "--", path)
			if err != nil {
				return false, false
			}
			if before == "" || before != after {
				return true, true
			}
		default:
			return false, false
		}
	}
	return false, true
}

// matchingFiles lists the regular files under root that any declaration matches, by
// slash-separated relative path. Literal declarations are looked up directly; wildcards walk the
// checkout. Symbolic links never count (a link could name a file outside the checkout), nor does
// anything under .git. truncated says the walk stopped at its bound.
func matchingFiles(root string, files []workflow.CompletionFile) (found map[string]bool, truncated bool) {
	found = map[string]bool{}
	walk := false
	for _, f := range files {
		if !f.Literal() {
			walk = true
			continue
		}
		if regularFile(root, f.Path) {
			found[f.Path] = true
		}
	}
	if !walk {
		return found, false
	}
	entries := 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable entry matches nothing
		}
		if entries++; entries > maxWalkedEntries {
			truncated = true
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
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		for _, f := range files {
			if f.Matches(rel) {
				found[rel] = true
				break
			}
		}
		return nil
	})
	return found, truncated
}

// regularFile reports whether rel names a regular file inside root, no component of which is a
// symbolic link.
func regularFile(root, rel string) bool {
	current := root
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&fs.ModeSymlink != 0 {
			return false
		}
		if i == len(parts)-1 {
			return info.Mode().IsRegular()
		}
		if !info.IsDir() || part == ".git" {
			return false
		}
	}
	return false
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

// committedBlob is the blob id of rel at commit, "" when the commit does not have it.
func committedBlob(ctx context.Context, root, commit, rel string) (string, error) {
	if commit == "" {
		return "", errors.New("no base commit recorded")
	}
	if _, err := git(ctx, root, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return "", err
	}
	out, err := git(ctx, root, "rev-parse", "--verify", "--quiet", commit+":"+rel)
	if err != nil {
		return "", nil // the path is not in the commit
	}
	return out, nil
}

func git(ctx context.Context, root string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
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
