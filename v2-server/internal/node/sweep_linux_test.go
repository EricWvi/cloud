//go:build linux

package node

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain runs the test binary as a stand-in Node when cleanup starts it as a root's nb: the
// runtime gives Node only its configuration path, so the stand-in cannot be chosen by flags.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "nb" {
		os.Exit(sweepStandIn())
	}
	os.Exit(m.Run())
}

// standInConfig is what a test writes as the root's configuration for the stand-in Node.
type standInConfig struct {
	// Mode is "kill" (kill what the ledger claims, then serve), "ignore" (serve without
	// killing anything), "fail" (exit before serving) or "hang" (ignore SIGTERM and never serve).
	Mode string `json:"mode"`
}

// sweepStandIn models Node's restart recovery for cleanup: it records that it ran, kills the
// processes in its root that its ledger claims (by the same rule cleanup uses to decide to start
// it), binds the control socket only after that, and serves until SIGTERM.
func sweepStandIn() int {
	config := os.Args[1]
	root := filepath.Dir(config)
	data, err := os.ReadFile(config)
	var opts standInConfig
	if err != nil || json.Unmarshal(data, &opts) != nil {
		return 2
	}
	starts, err := os.OpenFile(filepath.Join(root, "starts"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 2
	}
	_, _ = starts.WriteString("start\n")
	_ = starts.Close()
	switch opts.Mode {
	case "fail":
		return 1
	case "hang":
		signal.Ignore(syscall.SIGTERM)
		select {}
	}
	terminated := make(chan os.Signal, 1)
	signal.Notify(terminated, syscall.SIGTERM)
	if opts.Mode == "kill" {
		pids, err := processesInRoot(root)
		if err != nil {
			return 2
		}
		scope := func(attempt string) (markScope, bool) { return readMarkScope(filepath.Join(root, "n"), attempt) }
		for _, pid := range pids {
			if claimedByLedger(pid, observeProcess, scope) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
	listener, err := net.Listen("unix", ControlSocket(root))
	if err != nil {
		return 2
	}
	defer listener.Close()
	<-terminated
	return 0
}

func TestMarkedAttemptsReadsTheMarkEntryOnly(t *testing.T) {
	for _, tc := range []struct {
		name, environ string
		want          []string
	}{
		{"one attempt", "PATH=/bin\x00ORA_SESSION_ATTEMPT=a1\x00HOME=/h", []string{"a1"}},
		{"nested Node", "ORA_SESSION_ATTEMPT=outer:inner", []string{"outer", "inner"}},
		{"empty tokens", "ORA_SESSION_ATTEMPT=:a1:", []string{"a1"}},
		{"other variable", "XORA_SESSION_ATTEMPT=a1\x00ORA_SESSION_ATTEMPTS=a2", nil},
		{"no mark", "PATH=/bin", nil},
		{"cleared environment", "", nil},
	} {
		if got := markedAttempts([]byte(tc.environ)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: markedAttempts(%q) = %q, want %q", tc.name, tc.environ, got, tc.want)
		}
	}
}

func TestReadMarkScopeOnlyReadsThisLedgersRecords(t *testing.T) {
	home := t.TempDir()
	const attempt = "7d0c2a4e-5b7f-4c1e-9a43-0f6e2d1b8c90"
	writeMark(t, home, attempt, 42, 1000)
	if err := os.WriteFile(filepath.Join(home, "session-attempts", "0bad.mark"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, attempt string
		want          markScope
		ok            bool
	}{
		{"recorded attempt", attempt, markScope{Since: 42, UID: 1000}, true},
		{"unknown attempt", "11111111-2222-3333-4444-555555555555", markScope{}, false},
		{"damaged record", "0bad", markScope{}, false},
		{"path in token", "../session-attempts/" + attempt, markScope{}, false},
		{"empty token", "", markScope{}, false},
	} {
		if got, ok := readMarkScope(home, tc.attempt); got != tc.want || ok != tc.ok {
			t.Errorf("%s: readMarkScope(%q) = %+v, %v; want %+v, %v", tc.name, tc.attempt, got, ok, tc.want, tc.ok)
		}
	}
}

func TestClaimedByLedgerFollowsNodesRule(t *testing.T) {
	const uid = 1000
	scopes := map[string]markScope{"mine": {Since: 100, UID: uid}}
	scope := func(attempt string) (markScope, bool) { s, ok := scopes[attempt]; return s, ok }
	// Every case's process is pid 10; its parent chain is whatever the case lists.
	for _, tc := range []struct {
		name      string
		processes map[int]observed
		want      bool
	}{
		{"marked by this ledger", map[int]observed{10: {ppid: 1, start: 150, uid: uid, attempts: []string{"mine"}}}, true},
		{"nested under an outer Node's attempt", map[int]observed{10: {ppid: 1, start: 150, uid: uid, attempts: []string{"outer", "mine"}}}, true},
		{"marked by another root's Node", map[int]observed{10: {ppid: 1, start: 150, uid: uid, attempts: []string{"theirs"}}}, false},
		{"started before the attempt", map[int]observed{10: {ppid: 1, start: 99, uid: uid, attempts: []string{"mine"}}}, false},
		{"another user", map[int]observed{10: {ppid: 1, start: 150, uid: uid + 1, attempts: []string{"mine"}}}, false},
		{"unmarked orphan", map[int]observed{10: {ppid: 1, start: 150, uid: uid}}, false},
		{"unmarked child of a marked parent", map[int]observed{
			10: {ppid: 20, start: 150, uid: uid},
			20: {ppid: 1, start: 120, uid: uid, attempts: []string{"mine"}},
		}, true},
		{"grandchild of a marked process", map[int]observed{
			10: {ppid: 20, start: 150, uid: uid},
			20: {ppid: 30, start: 140, uid: uid},
			30: {ppid: 1, start: 120, uid: uid, attempts: []string{"mine"}},
		}, true},
		{"parent PID reused after the child started", map[int]observed{
			10: {ppid: 20, start: 150, uid: uid},
			20: {ppid: 1, start: 160, uid: uid, attempts: []string{"mine"}},
		}, false},
		{"parent gone", map[int]observed{10: {ppid: 20, start: 150, uid: uid}}, false},
		{"process gone", map[int]observed{}, false},
	} {
		lookup := func(pid int) (observed, bool) { p, ok := tc.processes[pid]; return p, ok }
		if got := claimedByLedger(10, lookup, scope); got != tc.want {
			t.Errorf("%s: claimedByLedger = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCleanupLetsTheRootsNodeKillItsMarkedLeftovers(t *testing.T) {
	paths := sweepRoot(t, "kill")
	const attempt = "7d0c2a4e-5b7f-4c1e-9a43-0f6e2d1b8c90"
	writeMark(t, paths.NodeHome, attempt, 0, uint32(os.Geteuid()))
	job := startJob(t, paths.Repositories, attempt)
	staleSocket(t, paths)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := cleanOwned(ctx, paths, 50*time.Millisecond); err != nil {
		t.Fatalf("cleanup with a leftover the root's Node kills: %v", err)
	}
	if job.alive() {
		t.Fatal("the marked job survived cleanup")
	}
	if got := sweepStarts(t, paths); got != 1 {
		t.Fatalf("Node started %d times for the sweep, want 1", got)
	}
	if pids, err := processesInRoot(paths.Root); err != nil || len(pids) != 0 {
		t.Fatalf("processes left in the root: %v %v", pids, err)
	}
	if _, err := os.Lstat(paths.Socket); !os.IsNotExist(err) {
		t.Fatalf("the sweeping Node's socket remains: %v", err)
	}
}

func TestCleanupDoesNotSweepLeftoversItsNodeDoesNotClaim(t *testing.T) {
	const attempt = "7d0c2a4e-5b7f-4c1e-9a43-0f6e2d1b8c90"
	for _, tc := range []struct {
		name string
		// mark records attempt's scope where the case wants it; job is the leftover's mark.
		mark func(t *testing.T, paths Paths)
		job  string
	}{
		{"unmarked", func(t *testing.T, paths Paths) { writeMark(t, paths.NodeHome, attempt, 0, uint32(os.Geteuid())) }, ""},
		{"another root's attempt", func(t *testing.T, _ Paths) { writeMark(t, t.TempDir(), attempt, 0, uint32(os.Geteuid())) }, attempt},
		{"started before the attempt", func(t *testing.T, paths Paths) {
			writeMark(t, paths.NodeHome, attempt, math.MaxUint64, uint32(os.Geteuid()))
		}, attempt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := sweepRoot(t, "kill")
			tc.mark(t, paths)
			job := startJob(t, paths.Repositories, tc.job)
			if err := cleanOwned(t.Context(), paths, 50*time.Millisecond); err == nil || !strings.Contains(err.Error(), "remains in workspace") {
				t.Fatalf("cleanup with an unclaimed leftover = %v, want it unconfirmed", err)
			}
			if !job.alive() {
				t.Fatal("cleanup killed a process its Node does not claim")
			}
			if got := sweepStarts(t, paths); got != 0 {
				t.Fatalf("Node started %d times for leftovers it does not claim", got)
			}
		})
	}
}

func TestCleanupStaysUnconfirmedWhenTheSweepLeavesTheJob(t *testing.T) {
	const attempt = "7d0c2a4e-5b7f-4c1e-9a43-0f6e2d1b8c90"
	for _, mode := range []string{"ignore", "fail"} {
		t.Run(mode, func(t *testing.T) {
			paths := sweepRoot(t, mode)
			writeMark(t, paths.NodeHome, attempt, 0, uint32(os.Geteuid()))
			job := startJob(t, paths.Repositories, attempt)
			if err := cleanOwned(t.Context(), paths, 50*time.Millisecond); err == nil || !strings.Contains(err.Error(), "remains in workspace") {
				t.Fatalf("cleanup after a sweep that left the job = %v, want it unconfirmed", err)
			}
			if !job.alive() {
				t.Fatal("cleanup killed the job itself")
			}
			if got := sweepStarts(t, paths); got != 1 {
				t.Fatalf("Node started %d times for the sweep, want 1", got)
			}
			if pids := ownedRunning(t, paths); len(pids) != 0 {
				t.Fatalf("the sweeping Node is still running: %v", pids)
			}
		})
	}
}

// sweepRoot claims a runtime root whose nb is the stand-in Node in mode, as a crashed
// incarnation leaves it: copied executable and configuration in place, nothing running.
func sweepRoot(t *testing.T, mode string) Paths {
	t.Helper()
	paths, lock, err := claimRoot(socketDirectory(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	for _, path := range []string{paths.NodeHome, paths.Repositories} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := copyAsset(testAsset(t, self), filepath.Join(paths.Root, "nb")); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(standInConfig{Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Config, config, 0600); err != nil {
		t.Fatal(err)
	}
	return paths
}

// writeMark records attempt's scope under nodeHome as Node does before it registers the attempt.
func writeMark(t *testing.T, nodeHome, attempt string, since uint64, uid uint32) {
	t.Helper()
	directory := filepath.Join(nodeHome, "session-attempts")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(markScope{Since: since, UID: uid})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, attempt+".mark"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

// standInJob is a leftover background job running in a checkout.
type standInJob struct{ done chan struct{} }

func (j standInJob) alive() bool {
	select {
	case <-j.done:
		return false
	default:
		return true
	}
}

// startJob starts a leftover job in dir carrying attempt's mark, or no mark when attempt is "".
// It is reaped as soon as it exits, so a killed job is not mistaken for a live one.
func startJob(t *testing.T, dir, attempt string) standInJob {
	t.Helper()
	cmd := exec.Command("/usr/bin/sleep", "60")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	if attempt != "" {
		cmd.Env = append(cmd.Env, attemptMark+"="+attempt)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	job := standInJob{done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(job.done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-job.done })
	return job
}

// staleSocket leaves the control socket a SIGKILLed Node could not remove.
func staleSocket(t *testing.T, paths Paths) {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: paths.Socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
}

// sweepStarts counts the stand-in Node's starts in paths' root.
func sweepStarts(t *testing.T, paths Paths) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(paths.Root, "starts"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "start\n")
}

// ownedRunning lists the processes running the root's owned executables.
func ownedRunning(t *testing.T, paths Paths) []int {
	t.Helper()
	pids, err := processesInRoot(paths.Root)
	if err != nil {
		t.Fatal(err)
	}
	var owned []int
	for _, pid := range pids {
		if exe, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe")); err == nil && ownedExecutable(paths.Root, exe) {
			owned = append(owned, pid)
		}
	}
	return owned
}

// A caller whose ctx ends while the sweeping Node starts must still get it stopped and reaped:
// RecoverRuntime releases the root's lock right after cleanup returns.
func TestCleanupReapsTheSweepingNodeWhenTheCallerGivesUp(t *testing.T) {
	const attempt = "7d0c2a4e-5b7f-4c1e-9a43-0f6e2d1b8c90"
	paths := sweepRoot(t, "hang")
	writeMark(t, paths.NodeHome, attempt, 0, uint32(os.Geteuid()))
	job := startJob(t, paths.Repositories, attempt)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if err := cleanOwned(ctx, paths, 50*time.Millisecond); err == nil {
		t.Fatal("cleanup succeeded although the sweeping Node never served and the job lives")
	}
	if pids := ownedRunning(t, paths); len(pids) != 0 {
		t.Fatalf("the sweeping Node is still running after cleanup returned: %v", pids)
	}
	if got := sweepStarts(t, paths); got != 1 {
		t.Fatalf("Node started %d times for the sweep, want 1", got)
	}
	if !job.alive() {
		t.Fatal("cleanup killed the job itself")
	}
}
