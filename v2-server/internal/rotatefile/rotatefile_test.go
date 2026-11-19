package rotatefile

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func openWriter(t *testing.T, dir string, limit int64, keep int) *Writer {
	t.Helper()
	w, err := New(filepath.Join(dir, "node.log"), limit, keep)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// Every byte written lands in exactly one file of the rotation chain: the current
// file plus the rotated ones, in order, hold the full stream with nothing dropped.
func TestWriterKeepsEveryByteAcrossRotations(t *testing.T) {
	dir := t.TempDir()
	w := openWriter(t, dir, 10, 3)
	var all []byte
	for i := 0; i < 40; i++ {
		chunk := []byte(strings.Repeat("x", 4) + "\n")
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
		all = append(all, chunk...)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got := readChain(t, filepath.Join(dir, "node.log"), 3)
	if string(got) != string(all) {
		t.Fatalf("reconstructed stream differs:\n got %d bytes\nwant %d bytes", len(got), len(all))
	}
	// The chain is bounded: no file beyond .<keep-1> exists.
	if _, err := os.Stat(filepath.Join(dir, "node.log.3")); !os.IsNotExist(err) {
		t.Fatalf("node.log.3 exists after bounded rotation: %v", err)
	}
}

// A write larger than the limit is written whole to the current file (a write
// cannot be split), and the next write rotates it away so nothing accumulates.
func TestWriterRotatesAfterAnOversizedWrite(t *testing.T) {
	dir := t.TempDir()
	w := openWriter(t, dir, 8, 3)
	big := []byte(strings.Repeat("y", 100))
	if _, err := w.Write(big); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("z")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readChain(t, filepath.Join(dir, "node.log"), 3); string(got) != string(big)+"z" {
		t.Fatalf("rotation chain lost the oversized write: got %d bytes, want %d", len(got), len(big)+1)
	}
}

// An over-limit file left by an earlier run rotates on the next write.
func TestWriterRotatesAnExistingOverLimitFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("z", 20)), 0600); err != nil {
		t.Fatal(err)
	}
	w := openWriter(t, dir, 10, 3)
	if _, err := w.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if got := readChain(t, path, 3); string(got) != strings.Repeat("z", 20)+"a" {
		t.Fatalf("rotation chain lost an over-limit pre-existing file: %q", got)
	}
}

// Concurrent writes (as os/exec's stdout and stderr copy goroutines issue) never
// corrupt a line; this runs under the race detector in the v2 gate.
func TestWriterConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	w := openWriter(t, dir, 1024, 3)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(seed byte) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				if _, err := w.Write([]byte{seed}); err != nil {
					t.Error(err)
					return
				}
			}
		}(byte('a' + g))
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	all := readChain(t, filepath.Join(dir, "node.log"), 3)
	if len(all) != 2000 {
		t.Fatalf("concurrent writes lost bytes: %d != 2000", len(all))
	}
}

func TestWriterRejectsWriteAfterClose(t *testing.T) {
	dir := t.TempDir()
	w := openWriter(t, dir, 1024, 3)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("write after close succeeded")
	}
}

func readChain(t *testing.T, path string, keep int) []byte {
	t.Helper()
	var all []byte
	for i := keep - 1; i >= 0; i-- {
		name := path
		if i > 0 {
			name += "." + string(rune('0'+i))
		}
		data, err := os.ReadFile(name)
		if err == nil {
			all = append(all, data...)
		} else if !os.IsNotExist(err) {
			t.Fatalf("read %s: %v", name, err)
		}
	}
	return all
}
