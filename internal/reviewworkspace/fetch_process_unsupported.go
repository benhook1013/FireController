//go:build !unix

package reviewworkspace

import "os/exec"

// CommandContext retains its platform's direct child cancellation. Native
// review execution already requires Unix-owned process-group support.
func configureFetchProcess(_ *exec.Cmd) func() { return func() {} }
