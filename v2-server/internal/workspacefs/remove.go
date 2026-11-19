package workspacefs

import (
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

// beforeOpenCheckout lets tests change the tree between the Lstat checks and the open.
var beforeOpenCheckout func(path string)

// Usage is how much a removed checkout held: the logical size of its regular files, and its
// entries of every kind (files, directories, links) below the checkout itself.
type Usage struct {
	Bytes int64
	Files int64
}

// RemoveCheckout deletes the checkout at path, which must lie strictly inside root (the project's
// checkout root, <project>/w/r). Every level from root down to path is checked with Lstat to be a
// directory and not a symlink; the checkout is then opened with os.OpenRoot and must be the very
// directory Lstat saw (same device and inode), so a swap between the check and the open is
// refused. Measuring, making directories writable for their owner and removing the contents all
// go through that root without following any link; the checkout directory itself goes last. A
// checkout or root that no longer exists counts as removed, with zero usage. The caller makes sure
// nothing else uses the checkout any more.
func RemoveCheckout(root, path string) (Usage, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return Usage{}, ErrUnsafeCheckout
	}
	root, path = filepath.Clean(root), filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return Usage{}, ErrUnsafeCheckout
	}
	at := root
	var info fs.FileInfo
	for i, part := range append([]string{""}, strings.Split(rel, string(filepath.Separator))...) {
		if i > 0 {
			at = filepath.Join(at, part)
		}
		info, err = os.Lstat(at)
		if errors.Is(err, fs.ErrNotExist) {
			return Usage{}, nil
		}
		if err != nil {
			return Usage{}, fmt.Errorf("inspect checkout: %w", err)
		}
		if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
			return Usage{}, ErrUnsafeCheckout
		}
	}
	if beforeOpenCheckout != nil {
		beforeOpenCheckout(path)
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		return Usage{}, fmt.Errorf("open checkout: %w", err)
	}
	defer r.Close()
	opened, err := r.Stat(".")
	if err != nil {
		return Usage{}, fmt.Errorf("inspect checkout: %w", err)
	}
	if !os.SameFile(info, opened) {
		return Usage{}, ErrUnsafeCheckout
	}
	var usage Usage
	var dirs []string
	walk := fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, err error) error {
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
			if info, err := d.Info(); err == nil {
				usage.Bytes += info.Size()
			}
		}
		return nil
	})
	if walk != nil {
		return Usage{}, fmt.Errorf("measure checkout: %w", walk)
	}
	// A directory without its owner's write permission cannot lose its entries. WalkDir reported
	// these as directories, not links, and the root keeps Chmod inside the checkout.
	for _, dir := range dirs {
		if i, err := r.Lstat(dir); err == nil && i.IsDir() && i.Mode().Perm()&0o700 != 0o700 {
			if err := r.Chmod(dir, i.Mode().Perm()|0o700); err != nil {
				return Usage{}, fmt.Errorf("make checkout writable: %w", err)
			}
		}
	}
	entries, err := fs.ReadDir(r.FS(), ".")
	if err != nil {
		return Usage{}, fmt.Errorf("list checkout: %w", err)
	}
	for _, e := range entries {
		if err := r.RemoveAll(e.Name()); err != nil {
			return Usage{}, fmt.Errorf("remove checkout: %w", err)
		}
	}
	// The emptied checkout itself, through its parent, which the Lstat walk above checked.
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return Usage{}, fmt.Errorf("open checkout parent: %w", err)
	}
	defer parent.Close()
	if again, err := parent.Lstat(filepath.Base(path)); err != nil || !os.SameFile(info, again) {
		return Usage{}, ErrUnsafeCheckout
	}
	if err := parent.Remove(filepath.Base(path)); err != nil {
		return Usage{}, fmt.Errorf("remove checkout: %w", err)
	}
	return usage, nil
}
