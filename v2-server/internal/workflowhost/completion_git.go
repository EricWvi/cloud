package workflowhost

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/gitexclude"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// The deliverable checks never run git against the checkout's own repository: its .git, config
// included, is the Agent's to write, and git honors settings there that run programs (filters,
// fsmonitor, hooks, a promisor remote's transport or core.sshCommand on a lazy fetch). Like
// workspacefs's delivery (deliveryGit), committed content is read from the project's mirror,
// which v2-server owns, and the checkout is listed through a scratch repository whose index holds
// the base tree, with the checkout's excludes copied in; git runs with a clean environment, no
// transport, and is killed with its process group when the check's time is up.

// deliverableCheckout is where a run's Agent nodes work, and the trusted repository that holds
// its base commit.
type deliverableCheckout struct {
	store.WorkspaceLocation
	// Mirror is the project's mirror; "" when it cannot be read, and then no committed content
	// is compared and wildcards list against an empty index.
	Mirror string
}

// maxDigestBytes bounds the content one deliverable digest reads; a larger file cannot be judged.
// Tests lower it.
var maxDigestBytes int64 = 64 << 20

// checkoutOf is the checkout a run's Agent nodes share, with its project's mirror.
func (h *Host) checkoutOf(ctx context.Context, tenant, runID string) (deliverableCheckout, error) {
	if h.runCheckout != nil {
		return h.runCheckout(ctx, tenant, runID)
	}
	at, err := h.store.WorkflowRunWorkspace(ctx, tenant, runID)
	if err != nil {
		return deliverableCheckout{}, err
	}
	c := deliverableCheckout{WorkspaceLocation: at}
	run, err := h.store.WorkflowRun(ctx, tenant, runID)
	if err == nil {
		var project store.Project
		if project, err = h.store.GetProject(ctx, execution.Scope{TenantID: tenant}, run.ProjectID); err == nil {
			c.Mirror = project.MirrorPath
		}
	}
	if err != nil {
		h.log.Warn("read the project mirror for the deliverable checks", "run", runID, "error", err)
	}
	return c, nil
}

// trustedGit runs git in dir with only env on top of a clean environment.
type trustedGit struct {
	dir string
	env []string
}

func (g trustedGit) command(ctx context.Context, args ...string) *exec.Cmd {
	config := []string{"--literal-pathspecs", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "core.splitIndex=false", "-c", "core.untrackedCache=false", "-c", "core.sparseCheckout=false",
		"-c", "core.attributesFile=/dev/null", "-c", "core.excludesFile=/dev/null",
		"-c", "credential.helper=", "-c", "gc.auto=0", "-c", "maintenance.auto=false",
		"-c", "core.quotepath=off", "-c", "protocol.allow=never"}
	cmd := exec.CommandContext(ctx, "git", append(config, args...)...)
	cmd.Dir = g.dir
	cmd.Env = append([]string{"PATH=/usr/bin:/bin", "HOME=/dev/null", "XDG_CONFIG_HOME=/dev/null", "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "GIT_OPTIONAL_LOCKS=0", "GIT_NO_REPLACE_OBJECTS=1",
		"GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL="}, g.env...)
	killGroupOnCancel(cmd)
	cmd.WaitDelay = time.Second
	return cmd
}

// value runs git and returns its trimmed output, at most 4 KiB.
func (g trustedGit) value(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := g.command(ctx, args...)
	cmd.Stdout = &limitWriter{w: &out, left: 4096}
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(out.String()), nil
}

// mirrorGit reads the project's mirror only.
func mirrorGit(mirror string) (trustedGit, error) {
	if mirror == "" || !filepath.IsAbs(mirror) {
		return trustedGit{}, errors.New("no project mirror")
	}
	return trustedGit{dir: mirror, env: []string{"GIT_DIR=" + mirror}}, nil
}

// committedDigest is fileDigest of rel's content at commit in the mirror, "" when the commit does
// not have it. The blob is read as stored (no filter applies to cat-file blob) and hashed here.
func committedDigest(ctx context.Context, mirror, commit, rel string) (string, error) {
	if !validCommit(commit) {
		return "", errors.New("no base commit recorded")
	}
	g, err := mirrorGit(mirror)
	if err != nil {
		return "", err
	}
	if kind, err := g.value(ctx, "cat-file", "-t", commit); err != nil || kind != "commit" {
		return "", errors.Join(errors.New("base commit is not in the mirror"), err)
	}
	if _, err := g.value(ctx, "cat-file", "-e", commit+":"+rel); err != nil {
		return "", nil // the path is not in the commit
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	h := sha256.New()
	cmd := g.command(ctx, "cat-file", "blob", commit+":"+rel)
	cmd.Stdout = &limitWriter{w: h, left: maxDigestBytes}
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func validCommit(commit string) bool {
	if len(commit) != 40 && len(commit) != 64 {
		return false
	}
	for _, c := range commit {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// gitListFiles feeds add the checkout's tracked and untracked, not ignored paths under prefixes,
// up to maxListedEntries; complete is false when the bound cut the listing short. "Tracked" is the
// base commit's tree, read from the mirror into a scratch index; without it every listed file is
// untracked. An error means the checkout could not be listed through git at all.
func gitListFiles(ctx context.Context, at deliverableCheckout, prefixes []string, add func(rel string)) (complete bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	scratch, err := os.MkdirTemp("", "ora-deliverables-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(scratch)
	format := "sha1"
	mirror, mirrorErr := mirrorGit(at.Mirror)
	if mirrorErr == nil {
		if f, err := mirror.value(ctx, "rev-parse", "--show-object-format"); err == nil {
			format = f
		} else {
			mirrorErr = err
		}
	}
	if _, err := (trustedGit{dir: scratch}).value(ctx, "init", "--bare", "--quiet", "--template=", "--object-format="+format); err != nil {
		return false, err
	}
	if err := copyExcludes(at.Path, scratch); err != nil {
		return false, err
	}
	env := []string{"GIT_DIR=" + scratch, "GIT_WORK_TREE=" + at.Path, "GIT_INDEX_FILE=" + filepath.Join(scratch, "index"),
		"GIT_OBJECT_DIRECTORY=" + filepath.Join(scratch, "objects")}
	if mirrorErr == nil {
		env = append(env, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+strconv.Quote(filepath.Join(at.Mirror, "objects")))
	}
	g := trustedGit{dir: at.Path, env: env}
	if mirrorErr == nil && validCommit(at.BaseCommit) {
		// Without the base tree, files the base tracks but an ignore rule covers go unlisted.
		_, _ = g.value(ctx, "read-tree", "--reset", at.BaseCommit)
	}

	args := []string{"ls-files", "-z", "--cached", "--others", "--exclude-standard", "--"}
	for _, p := range prefixes {
		if p == "" {
			p = "."
		}
		args = append(args, p)
	}
	cmd := g.command(ctx, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024)
	scanner.Split(splitNUL)
	entries, seen := 0, map[string]bool{}
	complete = true
	for scanner.Scan() {
		rel := scanner.Text()
		if seen[rel] {
			continue
		}
		seen[rel] = true
		if entries++; entries > maxListedEntries {
			complete = false
			cancel()
			break
		}
		add(rel)
	}
	_, _ = io.Copy(io.Discard, out)
	waitErr := cmd.Wait()
	if !complete {
		return false, nil
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return true, waitErr
}

// copyExcludes gives scratch the checkout's local Git excludes, read without running git; a
// checkout without .git has none.
func copyExcludes(checkout, scratch string) error {
	var rules [][]byte
	layout, err := gitexclude.Resolve(checkout)
	switch {
	case err == nil:
		if rules, err = layout.Excludes(); err != nil {
			return err
		}
	case errors.Is(err, os.ErrNotExist):
		if _, lstatErr := os.Lstat(filepath.Join(checkout, ".git")); !errors.Is(lstatErr, os.ErrNotExist) {
			return err
		}
	default:
		return err
	}
	dir := filepath.Join(scratch, "info")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "exclude"), bytes.Join(rules, []byte{'\n'}), 0o600)
}

func splitNUL(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// fileDigest hashes rel's content, opened beneath root so that no path swapped in after the checks
// can lead outside the checkout; only a regular file, at most maxDigestBytes, is read.
func fileDigest(root, rel string) (string, error) {
	if !regularFile(root, rel) {
		return "", fs.ErrNotExist
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer r.Close()
	// Non-blocking, so a FIFO swapped in since the checks cannot hold the open.
	f, err := r.OpenFile(filepath.FromSlash(rel), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return "", fs.ErrNotExist
	}
	h := sha256.New()
	if _, err := io.Copy(&limitWriter{w: h, left: maxDigestBytes}, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// errTooLarge stops a read past its bound.
var errTooLarge = errors.New("content exceeds the deliverable check's bound")

type limitWriter struct {
	w    io.Writer
	left int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.left {
		return 0, errTooLarge
	}
	l.left -= int64(len(p))
	return l.w.Write(p)
}
