//go:build linux

package node

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/rotatefile"
)

// A grandchild that inherits the child's stderr must not hold the child's Wait open. The child
// backgrounds a long sleep that keeps the pipe write end after the child exits; p.done must close
// when the child exits, not when the grandchild does (Node's reaper does exactly this, and the
// previous cmd.Stdout=Writer wiring let os/exec's copy goroutine keep cmd.Wait waiting for it).
func TestChildExitNotBlockedByGrandchildHoldingStderr(t *testing.T) {
	root := socketDirectory(t)
	paths := Paths{Root: root, UserHome: filepath.Join(root, "u")}
	if err := os.MkdirAll(paths.UserHome, 0700); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(root, "grandchild.pid")
	binary := filepath.Join(root, "nb")
	// The script backgrounds sleep, records its PID and exits; sleep keeps stderr (the pipe) open.
	script := "#!/bin/sh\nsleep 30 &\necho $! > " + pidFile + "\n"
	if err := os.WriteFile(binary, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	p, err := startChild(binary, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}()
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("child exit was blocked by a grandchild holding stderr")
	}
	if p.err != nil {
		t.Fatalf("child exit error: %v", p.err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := stopChild(ctx, p, time.Second); err != nil {
		t.Fatalf("stopChild: %v", err)
	}
}

// drainChildLog must keep reading after a write failure instead of stopping early, which would
// close the pipe's read end and break the child's stderr (the previous os/exec copy goroutine
// stopped on the first Write error, so the child took a SIGPIPE on its next stderr write).
func TestDrainChildLogKeepsReadingAfterWriteFailure(t *testing.T) {
	dir := socketDirectory(t)
	path := filepath.Join(dir, "nb.log")
	w, err := rotatefile.New(path, 64, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Force the rotate rename to fail even when the test runs as root (chmod is bypassed by root):
	// the rotation target already exists as a directory, so os.Rename(file, dir) fails with EISDIR.
	if err := os.Mkdir(path+".1", 0700); err != nil {
		t.Fatal(err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() { drainChildLog(pr, w); close(drained) }()
	// Write more than the pipe buffer: if the drain stopped reading after the first rotation
	// failure, these writes would EPIPE or block.
	const total = 128 * 1024
	errc := make(chan error, 1)
	go func() {
		chunk := bytes.Repeat([]byte("x"), 4096)
		for i := 0; i < total/len(chunk); i++ {
			if _, err := pw.Write(chunk); err != nil {
				errc <- err
				return
			}
		}
		_ = pw.Close()
		errc <- nil
	}()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("write failed: %v (the drain must keep reading)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("write blocked: the drain stopped reading after a write failure")
	}
	<-drained
}
