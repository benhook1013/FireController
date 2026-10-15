//go:build !unix

package reviewprocess

import "os/exec"

const processGroupsSupported = false

func configureOwnedProcess(_ *exec.Cmd) (func() error, error) {
	return nil, processError("configuration_invalid", false)
}
