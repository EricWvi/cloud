//go:build linux

package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A crashed Node leaves its Agents' background jobs running in the root, where cleanup refuses
// to call the root clean. Node itself kills them in its restart recovery (desktop#802): every
// process of a session attempt carries the attempt in ORA_SESSION_ATTEMPT, Node keeps the
// attempt's scope in <NodeHome>/session-attempts/<attempt>.mark, and before it publishes its
// control socket it kills every live process carrying the mark of an attempt its ledger has not
// confirmed stopped. Cleanup therefore lets the root's own Node do that once, instead of killing
// anything itself, and then checks the root exactly as strictly as before.

// attemptMark is the environment variable Node gives every process of a session attempt. Its
// value lists the attempt, after those of enclosing Nodes, separated by colons.
const attemptMark = "ORA_SESSION_ATTEMPT"

// sweepStartup bounds the sweeping Node's start, its restart recovery included. Each attempt's
// kill has a 5 s budget inside Node; normally the whole start takes about a second.
const sweepStartup = 20 * time.Second

// maxEnviron bounds how much of one process environment classification reads.
const maxEnviron = 1 << 20

// markScope is the part of an attempt's .mark record that decides which processes it claims:
// those of uid that started at or after since, in boot-relative clock ticks.
type markScope struct {
	Since uint64 `json:"since_ticks"`
	UID   uint32 `json:"uid"`
}

// observed is what classification needs to know about one process.
type observed struct {
	ppid  int
	start uint64
	uid   uint32
	// attempts lists the tokens of the process's attemptMark entry; nil when it has none or
	// its environment could not be read.
	attempts []string
}

// claimedByLedger reports whether the process pid is one that the restart recovery of the Node
// whose attempts scope describes kills: it carries the mark of one of those attempts, runs as
// the attempt's user and started inside its window, or its parent is such a process. This is
// Node's own rule; a process cleared of its environment is reached only through a live claimed
// parent, so an orphaned one is not, and neither is a process of another root's Node, whose
// attempts have no record here. A parent that started after its child is a reused PID and lends
// nothing. lookup returns false for a process that has gone or cannot be read.
func claimedByLedger(pid int, lookup func(int) (observed, bool), scope func(attempt string) (markScope, bool)) bool {
	var child *observed
	for hops := 0; hops < 64 && pid > 1; hops++ {
		p, ok := lookup(pid)
		if !ok || child != nil && p.start > child.start {
			return false
		}
		for _, attempt := range p.attempts {
			if s, ok := scope(attempt); ok && p.uid == s.UID && p.start >= s.Since {
				return true
			}
		}
		child, pid = &p, p.ppid
	}
	return false
}

// markedAttempts returns the tokens of the attemptMark entry in a NUL-separated environment.
func markedAttempts(environ []byte) []string {
	prefix := []byte(attemptMark + "=")
	for _, entry := range bytes.Split(environ, []byte{0}) {
		if value, ok := bytes.CutPrefix(entry, prefix); ok {
			var attempts []string
			for _, token := range strings.Split(string(value), ":") {
				if token != "" {
					attempts = append(attempts, token)
				}
			}
			return attempts
		}
	}
	return nil
}

// readMarkScope reads the scope Node recorded for attempt under nodeHome. An attempt token comes
// from a process environment, so anything but a plain file name is no attempt of this ledger.
func readMarkScope(nodeHome, attempt string) (markScope, bool) {
	if attempt == "" || len(attempt) > 128 || strings.Trim(attempt, "0123456789abcdefABCDEF-") != "" {
		return markScope{}, false
	}
	path := filepath.Join(nodeHome, "session-attempts", attempt+".mark")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1024 {
		return markScope{}, false
	}
	data, err := os.ReadFile(path)
	var scope markScope
	if err != nil || json.Unmarshal(data, &scope) != nil {
		return markScope{}, false
	}
	return scope, true
}

// observeProcess reads pid's parent, start tick, owner and attempt mark from procfs.
func observeProcess(pid int) (observed, bool) {
	proc := filepath.Join("/proc", strconv.Itoa(pid))
	data, err := os.ReadFile(filepath.Join(proc, "stat"))
	if err != nil {
		return observed{}, false
	}
	// comm is parenthesized and may itself contain spaces or parentheses.
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return observed{}, false
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return observed{}, false
	}
	ppid, ppidErr := strconv.Atoi(fields[1])                 // field 4
	start, startErr := strconv.ParseUint(fields[19], 10, 64) // field 22
	if ppidErr != nil || startErr != nil {
		return observed{}, false
	}
	info, err := os.Stat(proc)
	if err != nil {
		return observed{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return observed{}, false
	}
	p := observed{ppid: ppid, start: start, uid: stat.Uid}
	if file, err := os.Open(filepath.Join(proc, "environ")); err == nil {
		environ, readErr := io.ReadAll(io.LimitReader(file, maxEnviron))
		_ = file.Close()
		if readErr == nil {
			p.attempts = markedAttempts(environ)
		}
	}
	return p, true
}

// anyClaimedByLedger reports whether one of pids is a process the root's own Node would kill on
// its next start.
func anyClaimedByLedger(paths Paths, pids []int) bool {
	scope := func(attempt string) (markScope, bool) { return readMarkScope(paths.NodeHome, attempt) }
	for _, pid := range pids {
		if claimedByLedger(pid, observeProcess, scope) {
			return true
		}
	}
	return false
}

// sweepMarked starts the root's Node once, from the executable and configuration its last
// incarnation ran with, and stops it as soon as its control socket appears: Node binds it only
// after its restart recovery killed the marked processes of unconfirmed attempts. It signals
// nothing itself but the Node it started. The caller holds the root's lock and has stopped every
// owned process, so this Node is the only one on the root.
func sweepMarked(ctx context.Context, paths Paths, grace time.Duration) error {
	binary := filepath.Join(paths.Root, "nb")
	for _, path := range []string{binary, paths.Config} {
		if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("sweep needs the last incarnation's %s", filepath.Base(path))
		}
	}
	// The dead Node's socket would read as the new one being ready.
	if err := removeSockets(paths); err != nil {
		return err
	}
	slog.Info("runtime cleanup: starting Node to kill the leftover processes of its session attempts", "root", paths.Root)
	startup, cancel := context.WithTimeout(ctx, sweepStartup)
	defer cancel()
	p, err := startChild(binary, paths, paths.Config)
	if err != nil {
		return err
	}
	readyErr := waitSocket(startup, paths.Socket, p)
	return errors.Join(readyErr, stopChild(ctx, p, grace))
}
