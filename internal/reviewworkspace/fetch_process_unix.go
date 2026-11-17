//go:build unix

package reviewworkspace

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Git's HTTPS and index-pack descendants belong to this one command group.
// Cancellation and final cleanup must not leave them writing after its gate
// has been released. This mirrors the native review process ownership rule.
func configureFetchProcess(command *exec.Cmd) func() {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	kill := func() error {
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
	return func() { _ = kill() }
}
