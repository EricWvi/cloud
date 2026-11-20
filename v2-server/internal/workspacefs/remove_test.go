package workspacefs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) {
	t.Helper()
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRemoveCheckoutMeasuresAndRemovesOnlyTheCheckout(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "w", "r")
	checkout := filepath.Join(root, "c1")
	sibling := filepath.Join(root, "c2")
	writeTree(t, map[string]string{
		filepath.Join(checkout, "a.txt"):       "12345",
		filepath.Join(checkout, "dir", "b.go"): "123",
		filepath.Join(sibling, "keep.txt"):     "x",
	})
	outside := filepath.Join(base, "outside")
	writeTree(t, map[string]string{filepath.Join(outside, "secret"): "keep"})
	// A link inside the checkout pointing outside is removed as a link, never followed.
	if err := os.Symlink(outside, filepath.Join(checkout, "link")); err != nil {
		t.Fatal(err)
	}
	usage, err := RemoveCheckout(t.Context(), root, checkout)
	if err != nil {
		t.Fatal(err)
	}
	// a.txt, dir, dir/b.go and the link: 4 entries, 8 bytes of regular files.
	if usage != (Usage{Bytes: 8, Files: 4}) {
		t.Fatalf("usage = %+v, want 8 bytes in 4 entries", usage)
	}
	if _, err := os.Lstat(checkout); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkout still there: %v", err)
	}
	for _, kept := range []string{filepath.Join(sibling, "keep.txt"), filepath.Join(outside, "secret")} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("%s was touched: %v", kept, err)
		}
	}
	// Removing again is a no-op: the checkout counts as already reclaimed.
	if usage, err := RemoveCheckout(t.Context(), root, checkout); err != nil || usage != (Usage{}) {
		t.Fatalf("second removal = %+v, %v", usage, err)
	}
}

func TestRemoveCheckoutRefusesPathsOutsideTheRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "w", "r")
	victim := filepath.Join(base, "victim")
	writeTree(t, map[string]string{filepath.Join(victim, "f"): "keep", filepath.Join(root, "c", "f"): "x"})
	for _, path := range []string{root, victim, filepath.Join(root, "..", "victim"), "relative/c", base} {
		if _, err := RemoveCheckout(t.Context(), root, path); !errors.Is(err, ErrUnsafeCheckout) {
			t.Fatalf("RemoveCheckout(%q) = %v, want ErrUnsafeCheckout", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(victim, "f")); err != nil {
		t.Fatalf("victim touched: %v", err)
	}
}

func TestRemoveCheckoutRefusesSymlinks(t *testing.T) {
	cases := map[string]func(t *testing.T, base, root, victim string) string{
		"checkout itself": func(t *testing.T, base, root, victim string) string {
			link := filepath.Join(root, "c")
			if err := os.Symlink(victim, link); err != nil {
				t.Fatal(err)
			}
			return link
		},
		"parent directory": func(t *testing.T, base, root, victim string) string {
			if err := os.Symlink(base, filepath.Join(root, "p")); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(root, "p", "victim")
		},
		"checkout root": func(t *testing.T, base, root, victim string) string {
			if err := os.RemoveAll(root); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(base, root); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(root, "victim")
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "w", "r")
			victim := filepath.Join(base, "victim")
			writeTree(t, map[string]string{filepath.Join(victim, "f"): "keep"})
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			path := setup(t, base, root, victim)
			if _, err := RemoveCheckout(t.Context(), root, path); !errors.Is(err, ErrUnsafeCheckout) {
				t.Fatalf("err = %v, want ErrUnsafeCheckout", err)
			}
			if _, err := os.Stat(filepath.Join(victim, "f")); err != nil {
				t.Fatalf("victim touched: %v", err)
			}
		})
	}
}

func TestRemoveCheckoutMissingRootCountsAsRemoved(t *testing.T) {
	root := filepath.Join(t.TempDir(), "w", "r")
	if usage, err := RemoveCheckout(t.Context(), root, filepath.Join(root, "c")); err != nil || usage != (Usage{}) {
		t.Fatalf("got %+v, %v", usage, err)
	}
}

func TestRemoveCheckoutRemovesReadOnlyDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "w", "r")
	checkout := filepath.Join(root, "c")
	writeTree(t, map[string]string{filepath.Join(checkout, "locked", "inner", "f"): "x"})
	for _, dir := range []string{filepath.Join(checkout, "locked", "inner"), filepath.Join(checkout, "locked")} {
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RemoveCheckout(t.Context(), root, checkout); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(checkout); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkout still there: %v", err)
	}
}

func TestRemoveCheckoutRefusesADirectorySwappedAfterTheCheck(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "w", "r")
	checkout := filepath.Join(root, "c")
	elsewhere := filepath.Join(root, "other")
	writeTree(t, map[string]string{filepath.Join(checkout, "f"): "mine", filepath.Join(elsewhere, "f"): "keep"})
	defer SetBetweenLstatAndOpen(func(name string) {
		if name != "c" {
			return
		}
		// Between the Lstat and the open, the checkout is replaced by another directory.
		if err := os.Rename(checkout, checkout+".moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(elsewhere, checkout); err != nil {
			t.Fatal(err)
		}
	})()
	if _, err := RemoveCheckout(t.Context(), root, checkout); !errors.Is(err, ErrUnsafeCheckout) {
		t.Fatalf("err = %v, want ErrUnsafeCheckout", err)
	}
	if got, err := os.ReadFile(filepath.Join(checkout, "f")); err != nil || string(got) != "keep" {
		t.Fatalf("the swapped-in directory was touched: %q, %v", got, err)
	}
}

// A parent of the checkout is moved out of the root and a link to it put in its place: the
// removal must not follow the link out of the root, whether the swap lands before the walk
// reaches the parent or between the parent's Lstat and its open.
func TestRemoveCheckoutRefusesAParentSwappedForALinkOutOfTheRoot(t *testing.T) {
	for name, set := range map[string]func(func()) func(){
		"before the walk": func(swap func()) func() { return SetBeforeOpenCheckout(func(string) { swap() }) },
		"between Lstat and open": func(swap func()) func() {
			return SetBetweenLstatAndOpen(func(n string) {
				if n == "nested" {
					swap()
				}
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "w", "r")
			nested := filepath.Join(root, "nested")
			checkout := filepath.Join(nested, "c")
			writeTree(t, map[string]string{filepath.Join(checkout, "f"): "precious"})
			moved := filepath.Join(base, "outside")
			defer set(func() {
				if err := os.Rename(nested, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, nested); err != nil {
					t.Fatal(err)
				}
			})()
			if _, err := RemoveCheckout(t.Context(), root, checkout); !errors.Is(err, ErrUnsafeCheckout) {
				t.Fatalf("err = %v, want ErrUnsafeCheckout", err)
			}
			if got, err := os.ReadFile(filepath.Join(moved, "c", "f")); err != nil || string(got) != "precious" {
				t.Fatalf("a file outside the root was touched: %q, %v", got, err)
			}
		})
	}
}

// A file that cannot be measured fails the removal with its path, so it is retried; one that
// vanished since it was listed is skipped.
func TestRemoveCheckoutFailsWhenAnEntryCannotBeMeasured(t *testing.T) {
	root := filepath.Join(t.TempDir(), "w", "r")
	checkout := filepath.Join(root, "c")
	writeTree(t, map[string]string{filepath.Join(checkout, "a"): "12", filepath.Join(checkout, "b"): "345"})
	restore := SetEntryInfo(func(p string, d fs.DirEntry) (fs.FileInfo, error) {
		if p == "b" {
			return nil, syscall.EIO
		}
		return d.Info()
	})
	_, err := RemoveCheckout(t.Context(), root, checkout)
	restore()
	if !errors.Is(err, syscall.EIO) || !strings.Contains(err.Error(), "measure b") {
		t.Fatalf("err = %v, want EIO naming b", err)
	}
	if _, err := os.Stat(filepath.Join(checkout, "a")); err != nil {
		t.Fatalf("the checkout was removed although it could not be measured: %v", err)
	}
	defer SetEntryInfo(func(p string, d fs.DirEntry) (fs.FileInfo, error) {
		if p == "b" {
			return nil, fs.ErrNotExist
		}
		return d.Info()
	})()
	usage, err := RemoveCheckout(t.Context(), root, checkout)
	if err != nil || usage != (Usage{Bytes: 2, Files: 1}) {
		t.Fatalf("with a vanished entry: %+v, %v", usage, err)
	}
}

func TestRemoveCheckoutStopsWhenCancelled(t *testing.T) {
	root := filepath.Join(t.TempDir(), "w", "r")
	checkout := filepath.Join(root, "c")
	writeTree(t, map[string]string{filepath.Join(checkout, "a"): "1", filepath.Join(checkout, "b"): "2"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := RemoveCheckout(ctx, root, checkout); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(checkout); err != nil {
		t.Fatalf("a cancelled removal took the checkout: %v", err)
	}
}
