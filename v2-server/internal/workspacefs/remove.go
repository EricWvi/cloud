package workspacefs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnsafeCheckout reports a checkout that may not be removed: its recorded path is outside the
// checkout root, or the root or a directory on the way to it is a symlink or not a directory.
// Retrying cannot change that, so callers keep the directory and stop.
var ErrUnsafeCheckout = errors.New("checkout outside its root or behind a symlink")

// ErrCheckoutInUse reports a checkout a live session still holds, so it is not removed.
var ErrCheckoutInUse = errors.New("checkout still in use")

// Hooks for tests: beforeOpenCheckout runs after the checkout root is opened and before the walk
// down to the checkout; betweenLstatAndOpen runs inside openDir between its two steps; entryInfo
// stats one entry while measuring.
var (
	beforeOpenCheckout  func(path string)
	betweenLstatAndOpen func(name string)
	entryInfo           = func(_ string, d fs.DirEntry) (fs.FileInfo, error) { return d.Info() }
)

// Usage is how much a removed checkout held: the logical size of its regular files, and its
// entries of every kind (files, directories, links) below the checkout itself.
type Usage struct {
	Bytes int64
	Files int64
}

// openDir opens the directory name inside parent without following a link: name must be a
// directory by Lstat, and the directory opened must be that very one (same device and inode), so
// a swap between the two is refused. os.Root keeps the open inside parent in any case.
func openDir(parent *os.Root, name string) (*os.Root, fs.FileInfo, error) {
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return nil, nil, ErrUnsafeCheckout
	}
	if betweenLstatAndOpen != nil {
		betweenLstatAndOpen(name)
	}
	dir, err := parent.OpenRoot(name)
	if err != nil {
		// Not the directory Lstat saw any more (moved, or a link that os.Root refuses to follow out).
		if again, lerr := parent.Lstat(name); lerr != nil || !os.SameFile(info, again) {
			return nil, nil, ErrUnsafeCheckout
		}
		return nil, nil, err
	}
	opened, err := dir.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		dir.Close()
		if err == nil {
			err = ErrUnsafeCheckout
		}
		return nil, nil, err
	}
	return dir, info, nil
}

// RemoveCheckout deletes the checkout at path, which must lie strictly inside root (the project's
// checkout root, <project>/w/r). It opens root (not a link, checked by dev/inode like every other
// level), then walks down to the checkout through directory handles, each level opened inside the
// previous one without following links (openDir). Measuring, making directories writable for
// their owner and removing the contents go through the checkout's handle, and the emptied
// checkout is removed through its parent's handle, so no path is resolved again after the check:
// moving a parent away and putting a link in its place cannot redirect the removal. A checkout
// or root that no longer exists counts as removed, with zero usage. The caller makes sure nothing
// else uses the checkout any more.
//
// ctx is checked between the checkout's top-level entries and before the checkout itself goes:
// a cancelled removal leaves part of the checkout for a later attempt and returns ctx's error.
func RemoveCheckout(ctx context.Context, root, path string) (Usage, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return Usage{}, ErrUnsafeCheckout
	}
	root, path = filepath.Clean(root), filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return Usage{}, ErrUnsafeCheckout
	}
	gone := func(err error) (Usage, error) {
		if errors.Is(err, fs.ErrNotExist) {
			return Usage{}, nil
		}
		if errors.Is(err, ErrUnsafeCheckout) {
			return Usage{}, err
		}
		return Usage{}, fmt.Errorf("open checkout: %w", err)
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return gone(err)
	}
	if rootInfo.Mode()&fs.ModeSymlink != 0 || !rootInfo.IsDir() {
		return Usage{}, ErrUnsafeCheckout
	}
	parent, err := os.OpenRoot(root)
	if err != nil {
		return gone(err)
	}
	if opened, err := parent.Stat("."); err != nil || !os.SameFile(rootInfo, opened) {
		parent.Close()
		if err != nil {
			return gone(err)
		}
		return Usage{}, ErrUnsafeCheckout
	}
	if beforeOpenCheckout != nil {
		beforeOpenCheckout(path)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		next, _, err := openDir(parent, part)
		parent.Close()
		if err != nil {
			return gone(err)
		}
		parent = next
	}
	defer parent.Close()
	name := parts[len(parts)-1]
	checkout, info, err := openDir(parent, name)
	if err != nil {
		return gone(err)
	}
	defer checkout.Close()
	var usage Usage
	var dirs []string
	walk := fs.WalkDir(checkout.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		if p == "." {
			return nil
		}
		usage.Files++
		if d.Type().IsRegular() {
			info, err := entryInfo(p, d)
			if errors.Is(err, fs.ErrNotExist) {
				// Gone since it was listed: it holds nothing to release any more.
				usage.Files--
				return nil
			}
			if err != nil {
				return fmt.Errorf("measure %s: %w", p, err)
			}
			usage.Bytes += info.Size()
		}
		return nil
	})
	if walk != nil {
		return Usage{}, fmt.Errorf("measure checkout %s: %w", path, walk)
	}
	// A directory without its owner's write permission cannot lose its entries. WalkDir reported
	// these as directories, not links, and the root keeps Chmod inside the checkout.
	for _, dir := range dirs {
		if i, err := checkout.Lstat(dir); err == nil && i.IsDir() && i.Mode().Perm()&0o700 != 0o700 {
			if err := checkout.Chmod(dir, i.Mode().Perm()|0o700); err != nil {
				return Usage{}, fmt.Errorf("make checkout writable: %w", err)
			}
		}
	}
	entries, err := fs.ReadDir(checkout.FS(), ".")
	if err != nil {
		return Usage{}, fmt.Errorf("list checkout %s: %w", path, err)
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return Usage{}, fmt.Errorf("remove checkout %s: %w", path, err)
		}
		if err := checkout.RemoveAll(e.Name()); err != nil {
			return Usage{}, fmt.Errorf("remove checkout %s: %w", path, err)
		}
	}
	// The emptied checkout itself, through the parent handle the walk opened: it must still be the
	// directory that was emptied; a name swapped for anything else (a link included) is refused.
	if err := ctx.Err(); err != nil {
		return Usage{}, fmt.Errorf("remove checkout %s: %w", path, err)
	}
	if again, err := parent.Lstat(name); err != nil || !os.SameFile(info, again) {
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Usage{}, fmt.Errorf("remove checkout %s: %w", path, err)
		}
		return Usage{}, ErrUnsafeCheckout
	}
	if err := parent.Remove(name); err != nil {
		return Usage{}, fmt.Errorf("remove checkout %s: %w", path, err)
	}
	return usage, nil
}
