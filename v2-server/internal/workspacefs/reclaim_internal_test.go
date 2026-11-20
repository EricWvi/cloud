package workspacefs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/gitexclude"
)

// corrupt calls only Git's own object complaints unverifiable: a context that ended, or an I/O
// diagnostic before the complaint, stays retryable.
func TestCorruptClassifiesOnlyObjectDamage(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("git for-each-ref: %w", context.Canceled), false},
		{fmt.Errorf("git for-each-ref: %w: missing object", context.DeadlineExceeded), false},
		{errors.New("git ls-tree: exit status 128: error: unable to open loose object 12: Permission denied\nfatal: not a tree object"), false},
		{errors.New("git for-each-ref: exit status 128: fatal: missing object 12 for refs/heads/main"), true},
		{errors.New("git for-each-ref: exit status 128: fatal: unexpected line in .git/packed-refs: x"), true},
		{errors.New("git ls-tree: exit status 128: fatal: unable to read tree 12"), false},
	} {
		if got := errors.Is(corrupt(tc.err), ErrUnverifiable); got != tc.want {
			t.Errorf("corrupt(%q) unverifiable = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "init.defaultBranch=main"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A context that has ended while HEAD is resolved is returned as it is, whatever Git said.
func TestHeadPresentReturnsAnEndedContext(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-q", ".")
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "base")
	base := gitIn(t, root, "rev-parse", "HEAD")
	gitIn(t, root, "checkout", "-q", "--detach")
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "head")
	head := gitIn(t, root, "rev-parse", "HEAD")
	if err := os.Remove(filepath.Join(root, ".git", "objects", head[:2], head[2:])); err != nil {
		t.Fatal(err)
	}
	s, err := NewDeliveryFS(t.TempDir(), DefaultDeliveryObjectBytes)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	agent := deliveryGit{binary: s.git, dir: root, env: []string{"GIT_CEILING_DIRECTORIES=" + filepath.Dir(root)}}
	if _, err := s.headPresent(ctx, agent, &staged{root: root, base: base}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// A well-formed ref file that for-each-ref did not list is one Git could not read.
func TestCheckRecordedRefsRequiresListedRefs(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-q", ".")
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "base")
	layout, err := gitexclude.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkRecordedRefs(layout, map[string]bool{"refs/heads/main": true}); err != nil {
		t.Fatalf("listed: %v", err)
	}
	if err := checkRecordedRefs(layout, map[string]bool{}); !errors.Is(err, ErrUnverifiable) {
		t.Fatalf("not listed: %v", err)
	}
	if err := checkRecordedRefs(layout, nil); err != nil {
		t.Fatalf("format only: %v", err)
	}
}

func TestValidRefName(t *testing.T) {
	for name, want := range map[string]bool{
		"refs/heads/main": true, "refs/tags/v1.0": true,
		"refs/heads/main.lock": false, "refs/heads/.hidden": false, "refs/heads/a..b": false,
		"refs/heads/a b": false, "refs/heads/a~1": false, "refs/heads/a@{1}": false, "refs/heads/": false,
		"refs/heads/x.": false, "refs/heads/a\\b": false, "@": false,
	} {
		if got := validRefName(name); got != want {
			t.Errorf("validRefName(%q) = %v, want %v", name, got, want)
		}
	}
}
