// Package procgroup kills the process group a launched child leads.
package procgroup

import (
	"errors"
	"os/exec"
	"syscall"
)

// Kill sends SIGKILL to cmd's process group. A process already gone -- the
// child exited on its own between the caller's decision to kill it and this
// call -- is not an error: ESRCH means there is nothing left to kill, not that
// the kill failed.
func Kill(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
