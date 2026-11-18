//go:build linux

package agentruntime

import (
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/wanglongan587/cloud/v2-server/internal/gitexclude"
)

// excludeHeader introduces the rules Ora appends, so a person reading the file knows where they
// came from.
const excludeHeader = "# Skills Ora materialized for the Agent; not part of the project."

// excludeFromGit keeps paths Ora wrote into a checkout out of Git's untracked files, so an Agent's
// `git add -A` does not commit them and the workspace views show only the Agent's own changes.
// paths are slash-separated and relative to the checkout. The rules go to the checkout's own
// .git/info/exclude, never the project's .gitignore, and existing lines are kept; a path already
// listed is not added twice. Linked worktrees use the common Git directory.
func excludeFromGit(root *os.Root, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if _, err := root.Lstat(".git"); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	layout, err := gitexclude.Resolve(root.Name())
	if err != nil {
		return err
	}
	excludes, err := layout.Excludes()
	if err != nil {
		return err
	}
	existing := string(excludes[len(excludes)-1])
	have := map[string]bool{}
	inside := false
	for line := range strings.SplitSeq(existing, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == excludeHeader {
			inside = true
		}
		if inside {
			have[line] = true
		}
	}
	var add strings.Builder
	for _, p := range paths {
		rule := "/" + escapeGitPattern(p)
		if have[rule] {
			continue
		}
		have[rule] = true
		if add.Len() == 0 {
			if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
				add.WriteString("\n")
			}
			if !have[excludeHeader] {
				add.WriteString(excludeHeader + "\n")
			}
		}
		add.WriteString(rule + "\n")
	}
	if add.Len() == 0 {
		return nil
	}
	// One append per call: concurrent writers each add whole lines.
	//
	// This write follows checkout-controlled pointers (.git as a gitfile or symlink, and
	// commondir), which the Agent can rewrite to name any directory it likes. That is acceptable
	// only while the Agent and Cloud run as the same OS user (A1 D2), so it gains nothing it
	// could not do itself. Revisit before Agents are sandboxed: never follow checkout-controlled
	// pointers for writes then.
	return layout.Append(add.String())
}

// escapeGitPattern makes a path match itself literally in a gitignore rule.
func escapeGitPattern(p string) string {
	var b strings.Builder
	for _, r := range p {
		if strings.ContainsRune(`\*?[!# `, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// serviceExcludedPath reports whether `path` is listed under the service's own header in the
// checkout's `.git/info/exclude`. A real directory that only the service wrote (a previously
// materialized user skill) lands here, so a later market install may replace it; a project-owned
// path never does.
func serviceExcludedPath(root *os.Root, path string) bool {
	layout, err := gitexclude.Resolve(root.Name())
	if err != nil {
		return false
	}
	excludes, err := layout.Excludes()
	if err != nil {
		return false
	}
	rule := "/" + escapeGitPattern(path)
	for _, raw := range excludes {
		inside := false
		for line := range strings.SplitSeq(string(raw), "\n") {
			line = strings.TrimSuffix(line, "\r")
			if line == excludeHeader {
				inside = true
			} else if inside && line == rule {
				return true
			}
		}
	}
	return false
}
