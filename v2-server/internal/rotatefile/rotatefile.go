package rotatefile

import (
	"fmt"
	"os"
	"sync"
)

// Writer is an append-only file writer that rotates the current file once its
// size would cross limit, keeping at most keep files: `path`, `path.1`, ...,
// `path.<keep-1>`. Rotation only closes, renames and reopens, so no write is
// ever dropped and a caller's write-then-Sync ordering stays intact; a failure
// surfaces from Write or Sync instead of silently losing events. Writers are
// safe for concurrent use, which lets one Writer serve both of a child process's
// stdout and stderr (os/exec copies each stream from its own pipe goroutine).
type Writer struct {
	mu     sync.Mutex
	path   string
	limit  int64
	keep   int
	file   *os.File
	size   int64
	closed bool
}

// New opens path in append mode and returns a writer bounded to limit bytes per
// file. keep must be at least 2 (the current file plus one rotated file).
func New(path string, limit int64, keep int) (*Writer, error) {
	if keep < 2 {
		return nil, fmt.Errorf("rotatefile: keep must be at least 2")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Writer{path: path, limit: limit, keep: keep, file: f, size: info.Size()}, nil
}

// Write appends p, rotating first when it would cross the per-file limit. An
// over-limit file left by an earlier run rotates on the next write.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	if w.size+int64(len(p)) > w.limit {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// Sync flushes the current file, mirroring the durability a journal needs.
func (w *Writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	return w.file.Sync()
}

// Close closes the current file; later writes fail with os.ErrClosed.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.file == nil {
		return nil
	}
	return w.file.Close()
}

// rotateLocked shifts `path.(i-1)` up to `path.i` for i from keep-1 down to 1
// (dropping the oldest), then reopens `path` empty. The caller holds mu.
func (w *Writer) rotateLocked() error {
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return err
		}
		w.file = nil
	}
	for i := w.keep - 1; i >= 1; i-- {
		_ = os.Remove(w.path + fmt.Sprintf(".%d", i))
		_ = os.Rename(w.path+fmt.Sprintf(".%d", i-1), w.path+fmt.Sprintf(".%d", i))
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	w.file = f
	w.size = 0
	return nil
}
