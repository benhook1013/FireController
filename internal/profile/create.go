package profile

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"
)

// Create publishes a new private version-one configuration. An existing file is
// only read: a matching selection succeeds without changing any of its bytes.
// Credential references are validated as text and are never opened or resolved.
func Create(path string, binding Binding) (Context, *Error) {
	var zero Context
	selected := entry{ServerURL: binding.Context.ServerURL, InstanceID: binding.Context.InstanceID, ProjectID: binding.Context.ProjectID, WorkerID: binding.Context.WorkerID, CredentialFile: binding.CredentialFile}
	config := configuration{Version: 1, Profiles: map[string]entry{binding.Context.Profile: selected}}
	if err := validateConfiguration(config); err != nil {
		return zero, err
	}
	path, pathErr := setupPath(path, "profiles")
	if pathErr != nil {
		return zero, pathErr
	}
	data, err := json.Marshal(config)
	if err != nil || len(data)+1 > MaxConfigBytes {
		return zero, failure("profiles_too_large", "The profiles configuration exceeds the 1 MiB limit.", "Reduce the configuration size.")
	}
	data = append(data, '\n')
	match := func(existing []byte) *Error {
		old, err := decodeConfiguration(existing)
		if err != nil {
			return err
		}
		if candidate, ok := old.Profiles[binding.Context.Profile]; !ok || candidate != selected {
			return setupError("profiles", "conflict")
		}
		return nil
	}
	if err := publishSetupFile(path, data, 0o600, "profiles", match, syncSetupDirectory); err != nil {
		return zero, err
	}
	return binding.Context, nil
}

func setupPath(path, family string) (string, *Error) {
	if !utf8.ValidString(path) || strings.IndexByte(path, 0) >= 0 || !filepath.IsAbs(path) || filepath.Dir(filepath.Clean(path)) == filepath.Clean(path) {
		return "", setupError(family, "invalid_path")
	}
	return filepath.Clean(path), nil
}

func setupError(family, kind string) *Error {
	messages := map[string]string{
		"invalid_path":    "An explicit absolute file path is required.",
		"not_private":     "The selected file and its parent directory must be owner-private.",
		"not_regular":     "The selected file must be a regular file, not a link or special file.",
		"conflict":        "The existing file does not match the requested setup.",
		"write_failed":    "The private setup file could not be published.",
		"write_uncertain": "The setup file was published, but its directory synchronization could not be confirmed.",
	}
	return failure(family+"_"+kind, messages[kind], "Check the private setup location; retain the exact request when retrying.")
}

// setupParent creates missing directories without following directory symlinks.
// Existing ancestors need not be private, but the immediate parent must be.
func setupParent(path, family string) *Error {
	parent := filepath.Dir(path)
	var ancestors []string
	for current := parent; ; current = filepath.Dir(current) {
		ancestors = append(ancestors, current)
		if filepath.Dir(current) == current {
			break
		}
	}
	for i := len(ancestors) - 1; i >= 0; i-- {
		current := ancestors[i]
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err = os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return setupError(family, "write_failed")
			}
			if err == nil {
				if err = syncSetupDirectory(filepath.Dir(current)); err != nil {
					return setupError(family, "write_failed")
				}
			}
			info, err = os.Lstat(current)
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return setupError(family, "write_failed")
		}
		if i == 0 && runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return setupError(family, "not_private")
		}
	}
	return nil
}

func setupFileInfo(info os.FileInfo, family string) *Error {
	if !info.Mode().IsRegular() {
		return setupError(family, "not_regular")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return setupError(family, "not_private")
	}
	if family == "launcher" && info.Mode().Perm()&0o100 == 0 {
		return failure("launcher_invalid_executable", "The selected launcher must be executable by its owner.", "Use a private executable launcher file.")
	}
	return nil
}

func matchSetupFile(path, family string, match func([]byte) *Error) (bool, *Error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, setupError(family, "write_failed")
	}
	if err := setupFileInfo(info, family); err != nil {
		return true, err
	}
	f, err := os.Open(path)
	if err != nil {
		return true, setupError(family, "write_failed")
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return true, setupError(family, "write_failed")
	}
	if err := setupFileInfo(info, family); err != nil {
		return true, err
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	if err != nil || len(data) > MaxConfigBytes {
		return true, setupError(family, "conflict")
	}
	return true, match(data)
}

// publishSetupFile is shared only by the two local profile setup operations.
// The sync parameter permits focused failure proof without mutable global hooks.
func publishSetupFile(path string, data []byte, mode os.FileMode, family string, match func([]byte) *Error, syncDirectory func(string) error) *Error {
	if err := setupParent(path, family); err != nil {
		return err
	}
	if exists, err := matchSetupFile(path, family, match); exists {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".fc-setup-*")
	if err != nil {
		return setupError(family, "write_failed")
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	defer temp.Close()
	if err = temp.Chmod(mode); err == nil {
		_, err = temp.Write(data)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil || closeErr != nil {
		return setupError(family, "write_failed")
	}
	if err = os.Link(tempPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			exists, matchErr := matchSetupFile(path, family, match)
			if matchErr != nil {
				return matchErr
			}
			// A disappearing target does not prove this attempt published it.
			if !exists {
				return setupError(family, "write_failed")
			}
			return nil
		}
		return setupError(family, "write_failed")
	}
	// Publication has happened. Never remove the target on any later failure.
	if err := os.Remove(tempPath); err != nil {
		return setupError(family, "write_uncertain")
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return setupError(family, "write_uncertain")
	}
	return nil
}

func syncSetupDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil // Windows has no supported directory-fsync operation here.
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func shellLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
