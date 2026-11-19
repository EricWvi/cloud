//go:build !unix

package workflowhost

import "os/exec"

// killGroupOnCancel leaves cmd's default cancellation; WaitDelay still bounds the wait.
func killGroupOnCancel(*exec.Cmd) {}
