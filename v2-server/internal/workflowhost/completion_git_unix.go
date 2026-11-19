//go:build unix

package workflowhost

import (
	"os/exec"
	"syscall"
)

// killGroupOnCancel puts cmd in its own process group and kills the whole group when its context
// ends, so no helper git started (a transport, a fetch) outlives the check holding its pipes.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
