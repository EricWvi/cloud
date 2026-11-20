package workspacefs

import "sync/atomic"

// gitFault, when set, runs before every Git command this package starts and fails it with the
// error it returns; see InjectGitFault.
var gitFault atomic.Pointer[func(args []string) error]

// InjectGitFault makes fault decide, before each Git command this package runs, whether that
// command fails (with the returned error) instead of running. It exists for tests of callers that
// must tell a passing failure from a lasting one; the returned function removes it.
func InjectGitFault(fault func(args []string) error) (restore func()) {
	gitFault.Store(&fault)
	return func() { gitFault.Store(nil) }
}
