// Package gitexclude resolves the local Git excludes shared by skill materialization and delivery.
package gitexclude

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	maxPointerBytes = 4096
	maxExcludeBytes = 4 << 20
)

// Layout identifies a checkout's Git directory and, for a linked worktree, its shared directory.
type Layout struct {
	checkout, gitdir, common string
}

// Resolve reads Git directory pointers without invoking Git or loading repository configuration.
// .git may be a directory, a gitfile pointer, or a symlink to a directory; a symlink is resolved
// like a gitfile pointer. A missing .git returns os.ErrNotExist (check the entry itself with Lstat
// to tell absence from a pointer whose target is missing). Broken metadata returns an error
// distinct from absence.
func Resolve(checkout string) (Layout, error) {
	checkout, err := directory(checkout)
	if err != nil {
		return Layout{}, err
	}
	gitdir := filepath.Join(checkout, ".git")
	info, err := os.Lstat(gitdir)
	if err != nil {
		return Layout{}, err
	}
	switch {
	case info.IsDir():
	case info.Mode()&os.ModeSymlink != 0:
		// Git accepts a .git symlink to a repository directory. Like a gitfile pointer, its target
		// may lie anywhere, but must be a directory; reads below it keep their containment checks.
		gitdir, err = directory(gitdir)
		if err != nil {
			return Layout{}, fmt.Errorf("resolve .git symlink: %w", err)
		}
	case info.Mode().IsRegular():
		raw, err := readRegular(checkout, ".git", maxPointerBytes)
		if err != nil {
			return Layout{}, fmt.Errorf("read gitdir pointer: %w", err)
		}
		path, ok := strings.CutPrefix(string(raw), "gitdir: ")
		if !ok {
			return Layout{}, errors.New("invalid gitdir pointer")
		}
		gitdir, err = pointerDirectory(checkout, path)
		if err != nil {
			return Layout{}, fmt.Errorf("resolve gitdir: %w", err)
		}
	default:
		return Layout{}, errors.New("invalid .git entry")
	}

	layout := Layout{checkout: checkout, gitdir: gitdir, common: gitdir}
	if _, err := os.Lstat(filepath.Join(gitdir, "commondir")); errors.Is(err, os.ErrNotExist) {
		return layout, nil
	} else if err != nil {
		return Layout{}, err
	}
	raw, err := readRegular(gitdir, "commondir", maxPointerBytes)
	if err != nil {
		return Layout{}, fmt.Errorf("read commondir pointer: %w", err)
	}
	layout.common, err = pointerDirectory(gitdir, string(raw))
	return layout, err
}

func pointerDirectory(parent, value string) (string, error) {
	value = strings.TrimRight(value, "\r\n")
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("invalid Git directory pointer")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(parent, value)
	}
	return directory(value)
}

func directory(path string) (string, error) {
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("Git metadata is not in a directory")
	}
	return path, nil
}

func (l Layout) rooted(path, gitdir string) (root, rel string, err error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve Git exclude: %w", err)
	}
	for _, base := range []string{gitdir, l.gitdir, l.checkout} {
		if rel, err := filepath.Rel(base, resolved); err == nil && filepath.IsLocal(rel) {
			return base, rel, nil
		}
	}
	return "", "", errors.New("Git exclude resolves outside the checkout and Git directories")
}

// Excludes returns independently bounded rule files, with the shared exclude last.
// A genuinely absent file is represented by nil. A dangling or escaping link fails closed.
func (l Layout) Excludes() ([][]byte, error) {
	dirs := []string{l.gitdir}
	if l.common != l.gitdir {
		dirs = append(dirs, l.common)
	}
	rules := make([][]byte, 0, len(dirs))
	for _, dir := range dirs {
		info := filepath.Join(dir, "info")
		if _, err := os.Lstat(info); errors.Is(err, os.ErrNotExist) {
			rules = append(rules, nil)
			continue
		} else if err != nil {
			return nil, err
		}
		root, rel, err := l.rooted(info, dir)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(root, rel, "exclude")
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			rules = append(rules, nil)
			continue
		} else if err != nil {
			return nil, err
		}
		root, rel, err = l.rooted(path, dir)
		if err != nil {
			return nil, err
		}
		raw, err := readRegular(root, rel, maxExcludeBytes)
		if err != nil {
			return nil, err
		}
		rules = append(rules, raw)
	}
	return rules, nil
}

// Append writes new rules into Git's shared exclude, through the same containment checks as reads.
func (l Layout) Append(rules string) error {
	common, err := os.OpenRoot(l.common)
	if err != nil {
		return err
	}
	defer common.Close()
	if err := common.Mkdir("info", 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	root, rel, err := l.rooted(filepath.Join(l.common, "info"), l.common)
	if err != nil {
		return err
	}
	path := filepath.Join(root, rel, "exclude")
	if _, err := os.Lstat(path); err == nil {
		root, rel, err = l.rooted(path, l.common)
		if err != nil {
			return err
		}
	} else if errors.Is(err, os.ErrNotExist) {
		rel = filepath.Join(rel, "exclude")
	} else {
		return err
	}
	f, err := openRegular(root, rel, os.O_WRONLY|os.O_APPEND|os.O_CREATE)
	if err != nil {
		return err
	}
	_, err = io.WriteString(f, rules)
	return errors.Join(err, f.Close())
}

func readRegular(root, rel string, limit int64) ([]byte, error) {
	f, err := openRegular(root, rel, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(raw)) > limit {
		return nil, errors.New("Git metadata exceeds its size limit")
	}
	return raw, err
}

func openRegular(root, rel string, flags int) (*os.File, error) {
	at, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer at.Close()
	f, err := at.OpenFile(rel, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("Git metadata is not a regular file")
	}
	return f, nil
}

// CommonDir is the repository's common Git directory: the Git directory itself, or for a linked
// worktree the main repository's. Submodule repositories live under its modules/.
func (l Layout) CommonDir() string { return l.common }
