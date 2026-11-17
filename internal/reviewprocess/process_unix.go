//go:build unix

package reviewprocess

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

const processGroupsSupported = true

// The started child is its own process group leader. Cancellation and final
// cleanup target only that owned group, including inherited-pipe descendants.
func configureOwnedProcess(command *exec.Cmd) (func() error, error) {
	if command == nil || command.Process != nil {
		return nil, processError("configuration_invalid", false)
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	kill := func() error {
		// Start failures have no owned group. Never signal PID/group zero, which
		// denotes the caller's own process group rather than the review's group.
		if command.Process == nil || command.Process.Pid <= 0 {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.Cancel = kill
	return kill, nil
}
