package gitexclude_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/gitexclude"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "init.templateDir="}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// plainGitdir is a checkout with a .git directory holding exclude.
func plainGitdir(t *testing.T, exclude string) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, ".git", "info", "exclude"), exclude)
	return root
}

// TestResolveAndExcludesOverLayouts walks the checkout layouts Git and Agents can produce. Each
// case gives the exclude files read (nil for an absent one, shared exclude last) or the failure.
func TestResolveAndExcludesOverLayouts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) string
		want  []string // "<none>" for an absent exclude file
		err   string   // a substring of the failure, "ErrNotExist" for absence
	}{
		{name: "normal .git directory", want: []string{"normal\n"},
			setup: func(t *testing.T) string { return plainGitdir(t, "normal\n") }},
		{name: ".git directory without info", want: []string{"<none>"},
			setup: func(t *testing.T) string {
				root := t.TempDir()
				if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
				return root
			}},
		{name: "gitfile with an absolute pointer", want: []string{"absolute\n"},
			setup: func(t *testing.T) string {
				root, real := t.TempDir(), filepath.Join(t.TempDir(), "real")
				write(t, filepath.Join(real, "info", "exclude"), "absolute\n")
				write(t, filepath.Join(root, ".git"), "gitdir: "+real+"\n")
				return root
			}},
		{name: "gitfile with a relative pointer", want: []string{"relative\n"},
			setup: func(t *testing.T) string {
				root := t.TempDir()
				write(t, filepath.Join(root, ".realgit", "info", "exclude"), "relative\n")
				write(t, filepath.Join(root, ".git"), "gitdir: .realgit\n")
				return root
			}},
		{name: "linked worktree with commondir", want: []string{"worktree\n", "common\n"},
			setup: func(t *testing.T) string {
				main := t.TempDir()
				git(t, main, "init", "-q", ".")
				git(t, main, "commit", "-q", "--allow-empty", "-m", "base")
				linked := filepath.Join(t.TempDir(), "linked")
				git(t, main, "worktree", "add", "-q", "--detach", linked)
				write(t, filepath.Join(main, ".git", "info", "exclude"), "common\n")
				write(t, filepath.Join(main, ".git", "worktrees", "linked", "info", "exclude"), "worktree\n")
				return linked
			}},
		{name: "git init --separate-git-dir", want: []string{"separate\n"},
			setup: func(t *testing.T) string {
				root, real := t.TempDir(), filepath.Join(t.TempDir(), "separate.git")
				git(t, root, "init", "-q", "--separate-git-dir", real, ".")
				write(t, filepath.Join(real, "info", "exclude"), "separate\n")
				return root
			}},
		{name: "exclude symlinked inside the Git directory", want: []string{"inside\n"},
			setup: func(t *testing.T) string {
				root := t.TempDir()
				write(t, filepath.Join(root, ".git", "ora-exclude"), "inside\n")
				symlink(t, "../ora-exclude", filepath.Join(root, ".git", "info", "exclude"))
				return root
			}},
		{name: "info directory symlinked inside the checkout", want: []string{"info-link\n"},
			setup: func(t *testing.T) string {
				root := t.TempDir()
				write(t, filepath.Join(root, "ora-info", "exclude"), "info-link\n")
				symlink(t, filepath.Join(root, "ora-info"), filepath.Join(root, ".git", "info"))
				return root
			}},
		{name: "exclude symlink escaping the checkout", err: "outside the checkout",
			setup: func(t *testing.T) string {
				root, outside := t.TempDir(), filepath.Join(t.TempDir(), "exclude")
				write(t, outside, "secret\n")
				symlink(t, outside, filepath.Join(root, ".git", "info", "exclude"))
				return root
			}},
		{name: "dangling exclude symlink", err: "resolve Git exclude",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				symlink(t, filepath.Join(root, "missing"), filepath.Join(root, ".git", "info", "exclude"))
				return root
			}},
		{name: "gitfile pointing to a regular file", err: "not in a directory",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				write(t, filepath.Join(root, ".git"), "gitdir: /etc/passwd\n")
				return root
			}},
		{name: "gitfile without the gitdir prefix", err: "invalid gitdir pointer",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				write(t, filepath.Join(root, ".git"), "/somewhere\n")
				return root
			}},
		{name: "commondir is a FIFO", err: "not a regular file",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(filepath.Join(root, ".git", "commondir"), 0o644); err != nil {
					t.Fatal(err)
				}
				return root
			}},
		{name: "exclude is a FIFO", err: "not a regular file",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				if err := os.MkdirAll(filepath.Join(root, ".git", "info"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(filepath.Join(root, ".git", "info", "exclude"), 0o644); err != nil {
					t.Fatal(err)
				}
				return root
			}},
		{name: "oversized exclude", err: "size limit",
			setup: func(t *testing.T) string { return plainGitdir(t, strings.Repeat("x", 4<<20+1)) }},
		{name: "oversized gitfile pointer", err: "size limit",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				write(t, filepath.Join(root, ".git"), "gitdir: "+strings.Repeat("a", 4097))
				return root
			}},
		{name: "missing .git", err: "ErrNotExist",
			setup: func(t *testing.T) string { return t.TempDir() }},
		{name: ".git symlink to a repository directory", want: []string{"symlinked\n"},
			setup: func(t *testing.T) string {
				root, real := t.TempDir(), filepath.Join(t.TempDir(), "repo.git")
				write(t, filepath.Join(real, "info", "exclude"), "symlinked\n")
				symlink(t, real, filepath.Join(root, ".git"))
				return root
			}},
		{name: ".git symlink to a regular file", err: "not in a directory",
			setup: func(t *testing.T) string {
				root, file := t.TempDir(), filepath.Join(t.TempDir(), "file")
				write(t, file, "gitdir: /tmp\n")
				symlink(t, file, filepath.Join(root, ".git"))
				return root
			}},
		{name: "dangling .git symlink", err: "resolve .git symlink",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				symlink(t, filepath.Join(root, "gone"), filepath.Join(root, ".git"))
				return root
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tc.setup(t)
			type result struct {
				rules [][]byte
				err   error
			}
			done := make(chan result, 1)
			go func() {
				layout, err := gitexclude.Resolve(root)
				if err != nil {
					done <- result{nil, err}
					return
				}
				rules, err := layout.Excludes()
				done <- result{rules, err}
			}()
			var got result
			select {
			case got = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("reading Git metadata blocked")
			}
			switch {
			case tc.err == "ErrNotExist":
				if !errors.Is(got.err, os.ErrNotExist) {
					t.Fatalf("err = %v, want os.ErrNotExist", got.err)
				}
			case tc.err != "":
				if got.err == nil || !strings.Contains(got.err.Error(), tc.err) {
					t.Fatalf("err = %v, want one containing %q", got.err, tc.err)
				}
				if strings.Contains(got.err.Error(), "secret") {
					t.Fatalf("error carries file content: %v", got.err)
				}
			default:
				if got.err != nil {
					t.Fatal(got.err)
				}
				rules := make([]string, len(got.rules))
				for i, r := range got.rules {
					rules[i] = string(r)
					if r == nil {
						rules[i] = "<none>"
					}
				}
				if !reflect.DeepEqual(rules, tc.want) {
					t.Fatalf("excludes = %q, want %q", rules, tc.want)
				}
			}
		})
	}
}
