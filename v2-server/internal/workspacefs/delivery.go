package workspacefs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/gitexclude"
)

// DefaultDeliveryObjectBytes is the logical object budget for one snapshot.
const DefaultDeliveryObjectBytes = 64 << 20

// ErrDeliveryTooLarge reports a delivery whose new objects exceed the configured budget.
var ErrDeliveryTooLarge = errors.New("delivery objects too large")

// DeliveryRequest describes where to snapshot and where to publish the result.
type DeliveryRequest struct {
	Workspace                         Workspace
	MirrorPath, ProjectID, DeliveryID string
	Message                           string
	ObservedAt                        time.Time
}

// Snapshot describes the published commit. An empty Commit means the tree equals the base.
type Snapshot struct {
	Commit string
}

// DeliverySnapshot identifies a retained snapshot. An empty Commit denotes an empty delivery.
type DeliverySnapshot struct {
	MirrorPath, ProjectID, DeliveryID, BaseCommit, Commit string
}

// DeliveryFS owns snapshot serialization and bounded Git invocations.
type DeliveryFS struct {
	scratch, git string
	maxObjects   int64
	mu           sync.Mutex
	locks        map[string]*workspaceLock
}

type workspaceLock struct {
	token chan struct{}
	users int
}

// NewDeliveryFS configures snapshot plumbing using scratch space outside Agent checkouts.
func NewDeliveryFS(scratch string, maxObjects int64) (*DeliveryFS, error) {
	if !filepath.IsAbs(scratch) || maxObjects <= 0 {
		return nil, errors.New("invalid delivery filesystem configuration")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	git, err = filepath.Abs(git)
	if err != nil {
		return nil, err
	}
	return &DeliveryFS{scratch: scratch, git: git, maxObjects: maxObjects, locks: make(map[string]*workspaceLock)}, nil
}

// LockWorkspace serializes deliveries for the same canonical checkout. Hold it through the
// database write so snapshots on that checkout become visible in the order they were collected.
func (s *DeliveryFS) LockWorkspace(ctx context.Context, root string) (func(), error) {
	key, err := filepath.EvalSymlinks(root)
	if errors.Is(err, os.ErrNotExist) {
		key, err = filepath.Abs(root)
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	l := s.locks[key]
	if l == nil {
		l = &workspaceLock{token: make(chan struct{}, 1)}
		s.locks[key] = l
	}
	l.users++
	s.mu.Unlock()
	release := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		l.users--
		if l.users == 0 {
			delete(s.locks, key)
		}
	}
	select {
	case l.token <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-l.token
			release()
			return nil, err
		}
		return func() { <-l.token; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

func deliveryRef(project, delivery string) (string, error) {
	for _, id := range []string{project, delivery} {
		if len(id) != 36 {
			return "", errors.New("invalid delivery identity")
		}
		for i, c := range id {
			if i == 8 || i == 13 || i == 18 || i == 23 {
				if c == '-' {
					continue
				}
			} else if strings.ContainsRune("0123456789abcdef", c) {
				continue
			}
			return "", errors.New("invalid delivery identity")
		}
	}
	return "refs/ora/deliveries/" + project + "/" + delivery, nil
}

type deliveryGit struct {
	binary, dir string
	env         []string
}

func (g deliveryGit) command(ctx context.Context, args ...string) *exec.Cmd {
	config := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "core.splitIndex=false", "-c", "core.untrackedCache=false",
		"-c", "core.attributesFile=/dev/null", "-c", "core.excludesFile=/dev/null",
		"-c", "credential.helper=", "-c", "commit.gpgsign=false", "-c", "gc.auto=0",
		"-c", "maintenance.auto=false", "-c", "core.quotepath=off", "-c", "protocol.allow=never"}
	cmd := exec.CommandContext(ctx, g.binary, append(config, args...)...)
	cmd.Dir = g.dir
	cmd.Env = append([]string{"PATH=/usr/bin:/bin", "HOME=/dev/null", "XDG_CONFIG_HOME=/dev/null", "LC_ALL=C", "TZ=UTC",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "GIT_OPTIONAL_LOCKS=0", "GIT_NO_REPLACE_OBJECTS=1"}, g.env...)
	cmd.WaitDelay = time.Second
	return cmd
}

func (g deliveryGit) run(ctx context.Context, stdin io.Reader, limit int, args ...string) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := g.command(ctx, args...)
	out := &limitedBuffer{limit: limit}
	stderr := &limitedBuffer{limit: 8192}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &cancelOnLimit{out, cancel}, discardDiagnostics{stderr}
	err := cmd.Run()
	if out.over {
		return nil, ErrTooLarge
	}
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}

type discardDiagnostics struct{ *limitedBuffer }

func (d discardDiagnostics) Write(p []byte) (int, error) {
	if n := min(len(p), d.limit-d.Len()); n > 0 {
		_, _ = d.Buffer.Write(p[:n])
	}
	return len(p), nil
}

type cancelOnLimit struct {
	buffer *limitedBuffer
	cancel context.CancelFunc
}

func (w *cancelOnLimit) Write(p []byte) (int, error) {
	n, err := w.buffer.Write(p)
	if err != nil {
		w.cancel()
	}
	return n, err
}

func (g deliveryGit) value(ctx context.Context, args ...string) (string, error) {
	raw, err := g.run(ctx, nil, 4096, args...)
	return strings.TrimSpace(string(raw)), err
}

func (s *DeliveryFS) mirrorGit(mirror string) (deliveryGit, error) {
	if !filepath.IsAbs(mirror) {
		return deliveryGit{}, errors.New("invalid delivery mirror location")
	}
	return deliveryGit{binary: s.git, dir: mirror, env: []string{"GIT_DIR=" + mirror}}, nil
}

// ErrUnverifiable reports a checkout whose snapshot cannot be built because of what it holds
// (unreadable Git metadata, changes too large, skills Ora cannot tell apart), not because of a
// failing disk or mirror: retrying gives the same answer.
var ErrUnverifiable = errors.New("checkout snapshot cannot be verified")

// isIOError reports an operating system error, which a later attempt may not meet again.
func isIOError(err error) bool {
	var path *fs.PathError
	var link *os.LinkError
	var sys *os.SyscallError
	return errors.As(err, &path) || errors.As(err, &link) || errors.As(err, &sys)
}

// staged is a checkout's snapshot tree written into a scratch repository whose objects fall back
// to the mirror's. close removes the scratch repository.
type staged struct {
	git, mirror    deliveryGit
	scratch, root  string
	base, baseTree string
	tree           string
	close          func()
}

// stage builds the checkout's snapshot tree by the delivery rules: a temporary index initialized
// at the base, every file Git does not ignore, materialized skills removed. It writes objects only
// into a scratch repository. The caller must hold the checkout lock.
func (s *DeliveryFS) stage(ctx context.Context, ws Workspace, mirrorPath string, observedAt time.Time) (*staged, error) {
	root, err := ws.root()
	if err != nil {
		return nil, err
	}
	mirror, err := s.mirrorGit(mirrorPath)
	if err != nil {
		return nil, err
	}
	format, err := mirror.value(ctx, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, err
	}
	base := ws.BaseCommit
	if !validCommit(base) || (format != "sha1" && format != "sha256") ||
		(format == "sha1" && len(base) != 40) || (format == "sha256" && len(base) != 64) {
		return nil, errors.New("invalid delivery base")
	}
	kind, err := mirror.value(ctx, "cat-file", "-t", base)
	if err != nil {
		return nil, err
	}
	if kind != "commit" {
		return nil, errors.New("delivery base is not a commit")
	}
	baseTree, err := mirror.value(ctx, "rev-parse", "--verify", base+"^{tree}")
	if err != nil {
		return nil, err
	}
	scratch, err := os.MkdirTemp(s.scratch, "delivery-")
	if err != nil {
		return nil, err
	}
	st := &staged{mirror: mirror, scratch: scratch, root: root, base: base, baseTree: baseTree, close: func() { os.RemoveAll(scratch) }}
	fail := func(err error) (*staged, error) { st.close(); return nil, err }
	g := deliveryGit{binary: s.git, dir: scratch}
	if _, err := g.value(ctx, "init", "--bare", "--template=", "--object-format="+format); err != nil {
		return fail(err)
	}
	exclude, withoutGit, err := copyDeliveryExclude(root, scratch)
	if err != nil {
		if !errors.Is(err, ErrGone) && !isIOError(err) {
			// Git metadata Ora may not read (an exclude outside the checkout, an oversized or
			// irregular file): what the checkout holds cannot be told, now or on a retry.
			err = fmt.Errorf("%w: %w", ErrUnverifiable, err)
		}
		return fail(err)
	}
	g.dir = root
	g.env = []string{"GIT_DIR=" + scratch, "GIT_WORK_TREE=" + root, "GIT_INDEX_FILE=" + filepath.Join(scratch, "delivery.index"),
		"GIT_OBJECT_DIRECTORY=" + filepath.Join(scratch, "objects"),
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + strconv.Quote(filepath.Join(mirrorPath, "objects")),
		"GIT_AUTHOR_NAME=Ora", "GIT_COMMITTER_NAME=Ora", "GIT_AUTHOR_EMAIL=ora@ora.invalid", "GIT_COMMITTER_EMAIL=ora@ora.invalid"}
	if !observedAt.IsZero() {
		at := observedAt.UTC().Format(time.RFC3339)
		g.env = append(g.env, "GIT_AUTHOR_DATE="+at, "GIT_COMMITTER_DATE="+at)
	}
	if _, err := g.value(ctx, "read-tree", "--reset", base); err != nil {
		return fail(err)
	}
	if err := checkDeliveryFileBytes(ctx, g, root, s.maxObjects); err != nil {
		return fail(err)
	}
	if _, err := g.value(ctx, "add", "-A", "--", "."); err != nil {
		return fail(err)
	}
	for _, rules := range exclude {
		if err := removeMaterializedPaths(ctx, g, base, rules); err != nil {
			return fail(err)
		}
	}
	if withoutGit {
		if err := requireNoMaterializedChanges(ctx, g, base); err != nil {
			return fail(err)
		}
	}
	if st.tree, err = g.value(ctx, "write-tree"); err != nil {
		return fail(err)
	}
	st.git = g
	return st, nil
}

// Deliver stages and publishes one snapshot. Its caller must hold the checkout lock.
func (s *DeliveryFS) Deliver(ctx context.Context, req DeliveryRequest) (Snapshot, error) {
	ref, err := deliveryRef(req.ProjectID, req.DeliveryID)
	if err != nil {
		return Snapshot{}, err
	}
	st, err := s.stage(ctx, req.Workspace, req.MirrorPath, req.ObservedAt)
	if err != nil {
		return Snapshot{}, err
	}
	defer st.close()
	g, mirror, scratch, base, tree := st.git, st.mirror, st.scratch, st.base, st.tree
	if tree == st.baseTree {
		return Snapshot{}, nil
	}
	raw, err := g.run(ctx, strings.NewReader(req.Message+"\n"), 1024, "commit-tree", tree, "-p", base)
	if err != nil {
		return Snapshot{}, err
	}
	commit := strings.TrimSpace(string(raw))
	if !validCommit(commit) {
		return Snapshot{}, errors.New("git returned an invalid delivery commit")
	}
	if err := s.importObjects(ctx, g, mirror, scratch, base, commit); err != nil {
		return Snapshot{}, err
	}
	if _, err := mirror.value(ctx, "update-ref", "--no-deref", ref, commit, strings.Repeat("0", len(base))); err != nil {
		// An identical pre-existing ref still represents this exact publication. Any other OID
		// belongs to another publication and must never be replaced.
		probe, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if existing, check := mirror.value(probe, "show-ref", "--verify", "--hash", ref); check != nil || existing != commit {
			return Snapshot{}, err
		}
	}
	return Snapshot{Commit: commit}, nil
}

// checkDeliveryFileBytes rejects oversized worktree input before Git writes any blobs.
func checkDeliveryFileBytes(ctx context.Context, g deliveryGit, root string, remaining int64) error {
	tree, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer tree.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := g.command(ctx, "ls-files", "-z", "-m", "-o", "--exclude-standard")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	waited := false
	defer func() {
		if !waited {
			cancel()
			_ = cmd.Wait()
		}
	}()
	reader := bufio.NewReader(out)
	for {
		name, err := reader.ReadString(0)
		if err == io.EOF && name == "" {
			waited = true
			return cmd.Wait()
		}
		if err != nil {
			return err
		}
		info, err := tree.Lstat(strings.TrimSuffix(name, "\x00"))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue // Git lists tracked deletions as modifications.
		}
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			if info.Size() > remaining {
				return ErrDeliveryTooLarge
			}
			remaining -= info.Size()
		}
	}
}

// copyDeliveryExclude copies the checkout's local excludes into the scratch repository and returns
// them, one entry per exclude file. withoutGit reports a checkout whose .git is gone (the Agent
// removed it): delivery needs no Agent repository, so it proceeds with no local rules.
func copyDeliveryExclude(root, scratch string) (rules [][]byte, withoutGit bool, err error) {
	layout, err := gitexclude.Resolve(root)
	if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Stat(root); errors.Is(statErr, os.ErrNotExist) {
			return nil, false, ErrGone
		}
		// Only an absent .git entry means "no rules"; a pointer whose target is missing is
		// broken metadata and still fails.
		if _, lstatErr := os.Lstat(filepath.Join(root, ".git")); errors.Is(lstatErr, os.ErrNotExist) {
			withoutGit, err = true, nil
		}
	}
	if err != nil {
		return nil, false, err
	}
	if !withoutGit {
		if rules, err = layout.Excludes(); err != nil {
			return nil, false, err
		}
	}
	dir := filepath.Join(scratch, "info")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, false, err
	}
	return rules, withoutGit, os.WriteFile(filepath.Join(dir, "exclude"), bytes.Join(rules, []byte{'\n'}), 0600)
}

// materializedRoots are where Ora writes skills into a checkout (see agentruntime).
var materializedRoots = []string{".opencode/skills", ".claude/skills"}

// ErrMaterializedUnknown fails a delivery that cannot tell skills Ora materialized from the
// Agent's own files: the record of Ora's paths lives in the checkout's Git exclude, which is gone.
var ErrMaterializedUnknown = errors.New("checkout has no Git directory and changes where Ora materializes skills")

// requireNoMaterializedChanges fails closed when, without the exclude that records Ora's paths,
// anything under a skill directory differs from the base: it could be a materialized (possibly
// private) skill. A checkout without such changes delivers normally.
func requireNoMaterializedChanges(ctx context.Context, git deliveryGit, base string) error {
	args := []string{"diff", "--cached", "--name-only", "--no-renames", "-z", base, "--"}
	for _, dir := range materializedRoots {
		args = append(args, ":(literal)"+dir)
	}
	changed, err := git.run(ctx, nil, MaxDiffBytes, args...)
	if err != nil {
		return err
	}
	if len(changed) > 0 {
		return ErrMaterializedUnknown
	}
	return nil
}

const materializedExcludeHeader = "# Skills Ora materialized for the Agent; not part of the project."

func removeMaterializedPaths(ctx context.Context, git deliveryGit, base string, exclude []byte) error {
	inside := false
	for line := range strings.SplitSeq(string(exclude), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == materializedExcludeHeader {
			inside = true
			continue
		}
		if !inside || !strings.HasPrefix(line, "/") {
			continue
		}
		// The projection writes rooted literal paths with Git's metacharacters escaped.
		var name strings.Builder
		escaped, literal := false, true
		for _, ch := range line[1:] {
			switch {
			case escaped:
				name.WriteRune(ch)
				escaped = false
			case ch == '\\':
				escaped = true
			case strings.ContainsRune("*?[", ch):
				literal = false
			default:
				name.WriteRune(ch)
			}
		}
		path := name.String()
		if !literal || escaped || path == "" {
			continue
		}
		pathspec := ":(literal)" + path
		previous, err := git.run(ctx, nil, MaxDiffBytes, "ls-tree", "-r", "--name-only", "-z", base, "--", pathspec)
		if err != nil {
			return err
		}
		basePaths := map[string]bool{}
		for p := range strings.SplitSeq(string(previous), "\x00") {
			basePaths[p] = true
		}
		staged, err := git.run(ctx, nil, MaxDiffBytes, "ls-files", "-z", "--", pathspec)
		if err != nil {
			return err
		}
		var remove strings.Builder
		for p := range strings.SplitSeq(string(staged), "\x00") {
			if !basePaths[p] {
				remove.WriteString(p)
				remove.WriteByte(0)
			}
		}
		if remove.Len() != 0 {
			if _, err := git.run(ctx, strings.NewReader(remove.String()), 4096, "update-index", "--force-remove", "-z", "--stdin"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *DeliveryFS) importObjects(ctx context.Context, git, mirror deliveryGit, scratch, base, commit string) error {
	list, err := os.Create(filepath.Join(scratch, "objects.list"))
	if err != nil {
		return err
	}
	defer list.Close()
	cmd := git.command(ctx, "rev-list", "--objects", "--no-object-names", commit, "^"+base)
	cmd.Stdout = list
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("enumerate delivery objects: %w", err)
	}
	if _, err := list.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := checkDeliveryObjectBytes(ctx, git, list, s.maxObjects); err != nil {
		return err
	}
	if _, err := list.Seek(0, io.SeekStart); err != nil {
		return err
	}
	pack, err := os.Create(filepath.Join(scratch, "delivery.pack"))
	if err != nil {
		return err
	}
	defer pack.Close()
	cmd = git.command(ctx, "pack-objects", "--stdout")
	cmd.Stdin, cmd.Stdout = list, pack
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pack delivery objects: %w", err)
	}
	if _, err := pack.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := mirror.run(ctx, pack, 4096, "index-pack", "--stdin", "--strict"); err != nil {
		return err
	}
	kind, err := mirror.value(ctx, "cat-file", "-t", commit)
	if err == nil && kind != "commit" {
		err = errors.New("delivery pack did not contain a commit")
	}
	return err
}

func checkDeliveryObjectBytes(ctx context.Context, git deliveryGit, objects io.Reader, limit int64) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := git.command(ctx, "cat-file", "--batch-check=%(objectsize)")
	cmd.Stdin = objects
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	scan := bufio.NewScanner(out)
	for scan.Scan() {
		size, err := strconv.ParseInt(scan.Text(), 10, 64)
		if err != nil || size < 0 {
			cancel()
			_ = cmd.Wait()
			return errors.New("invalid delivery object size")
		}
		if size > limit {
			cancel()
			_ = cmd.Wait()
			return ErrDeliveryTooLarge
		}
		limit -= size
	}
	return errors.Join(scan.Err(), cmd.Wait())
}
