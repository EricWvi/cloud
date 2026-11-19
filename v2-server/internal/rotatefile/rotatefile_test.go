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
// file plus the rotated ones hold the most recent bounded window of the stream,
// in order, with no gaps and nothing dropped by a rotation. Older data beyond
// keep files is discarded by design (the #1191 budget).
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
	want := all[len(all)-len(got):]
	if string(got) != string(want) {
		t.Fatalf("retained window differs:\n got %d bytes\nwant tail %d bytes", len(got), len(want))
	}
	if len(got) > 3*10 {
		t.Fatalf("retained %d bytes exceeds the keep x limit window", len(got))
	}
	// Rotation actually happened and bounded the current file, not merely kept bytes.
	if info, err := os.Stat(filepath.Join(dir, "node.log.1")); err != nil {
		t.Fatalf("expected node.log.1 after rotation: %v", err)
	} else if info.Size() == 0 {
		t.Fatal("node.log.1 is empty; rotation moved nothing")
	}
	if info, err := os.Stat(filepath.Join(dir, "node.log")); err != nil {
		t.Fatal(err)
	} else if info.Size() > 10 {
		t.Fatalf("current file %d bytes exceeds the 10-byte limit", info.Size())
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
	if info, err := os.Stat(filepath.Join(dir, "node.log.1")); err != nil {
		t.Fatalf("expected node.log.1 after the oversized write rotated: %v", err)
	} else if info.Size() != int64(len(big)) {
		t.Fatalf("node.log.1 holds %d bytes, want %d", info.Size(), len(big))
	}
	if info, err := os.Stat(filepath.Join(dir, "node.log")); err != nil {
		t.Fatal(err)
	} else if info.Size() != 1 {
		t.Fatalf("current file holds %d bytes, want 1", info.Size())
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
	if info, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected node.log.1 after rotating the over-limit file: %v", err)
	} else if info.Size() != 20 {
		t.Fatalf("node.log.1 holds %d bytes, want 20", info.Size())
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Size() != 1 {
		t.Fatalf("current file holds %d bytes, want 1", info.Size())
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
