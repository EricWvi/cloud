package workspacefs

import (
	"errors"
	"os"
	"path/filepath"
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
	usage, err := RemoveCheckout(root, checkout)
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
	if usage, err := RemoveCheckout(root, checkout); err != nil || usage != (Usage{}) {
		t.Fatalf("second removal = %+v, %v", usage, err)
	}
}

func TestRemoveCheckoutRefusesPathsOutsideTheRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "w", "r")
	victim := filepath.Join(base, "victim")
	writeTree(t, map[string]string{filepath.Join(victim, "f"): "keep", filepath.Join(root, "c", "f"): "x"})
	for _, path := range []string{root, victim, filepath.Join(root, "..", "victim"), "relative/c", base} {
		if _, err := RemoveCheckout(root, path); !errors.Is(err, ErrUnsafeCheckout) {
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
			if _, err := RemoveCheckout(root, path); !errors.Is(err, ErrUnsafeCheckout) {
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
	if usage, err := RemoveCheckout(root, filepath.Join(root, "c")); err != nil || usage != (Usage{}) {
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
	if _, err := RemoveCheckout(root, checkout); err != nil {
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
	defer SetBeforeOpenCheckout(func(path string) {
		// Between the Lstat walk and the open, the checkout is replaced by another directory.
		if err := os.Rename(path, path+".moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(elsewhere, path); err != nil {
			t.Fatal(err)
		}
	})()
	if _, err := RemoveCheckout(root, checkout); !errors.Is(err, ErrUnsafeCheckout) {
		t.Fatalf("err = %v, want ErrUnsafeCheckout", err)
	}
	if got, err := os.ReadFile(filepath.Join(checkout, "f")); err != nil || string(got) != "keep" {
		t.Fatalf("the swapped-in directory was touched: %q, %v", got, err)
	}
}
