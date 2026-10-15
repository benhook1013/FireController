package profile

import (
	"bytes"
	"os"
	"os/exec"
	"runtime"
)

// CreateLauncher publishes a POSIX shell launcher with a fixed local profile.
// It loads only configuration; the referenced credential is never opened.
func CreateLauncher(output, profilesPath, name, executable string) (Context, *Error) {
	var zero Context
	switch runtime.GOOS {
	case "aix", "android", "darwin", "dragonfly", "freebsd", "illumos", "ios", "linux", "netbsd", "openbsd", "solaris":
	default:
		return zero, failure("launcher_unsupported", "Shell launcher creation is unsupported on this host.", "Use an explicit profile file and selector instead.")
	}
	var err *Error
	if output, err = setupPath(output, "launcher"); err != nil {
		return zero, err
	}
	if profilesPath, err = setupPath(profilesPath, "launcher"); err != nil {
		return zero, err
	}
	if executable, err = setupPath(executable, "launcher"); err != nil {
		return zero, err
	}
	context, err := Load(profilesPath, name)
	if err != nil {
		return zero, err
	}
	info, statErr := os.Stat(executable)
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return zero, failure("launcher_invalid_executable", "The selected executable must be a regular executable file.", "Select an absolute path to the fc executable.")
	}
	// The path is already absolute, so LookPath checks effective execute
	// access without searching PATH or running the selected program.
	if _, err := exec.LookPath(executable); err != nil {
		return zero, failure("launcher_invalid_executable", "The selected executable must be a regular executable file.", "Select an absolute path to the fc executable.")
	}
	body := []byte("#!/bin/sh\nexec " + shellLiteral(executable) + " --profiles-file " + shellLiteral(profilesPath) + " --profile " + shellLiteral(name) + " \"$@\"\n")
	if len(body) > MaxConfigBytes {
		return zero, setupError("launcher", "write_failed")
	}
	match := func(existing []byte) *Error {
		if !bytes.Equal(existing, body) {
			return setupError("launcher", "conflict")
		}
		return nil
	}
	if err := publishSetupFile(output, body, 0o700, "launcher", match, syncSetupDirectory); err != nil {
		return zero, err
	}
	return context, nil
}
