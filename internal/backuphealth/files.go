package backuphealth

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/benhook1013/FireController/internal/strictjson"
)

var ErrInvalid = errors.New("backup health report is unavailable or invalid")

func safePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n")
}

func ValidateFiles(paths []string) error {
	if len(paths) > MaxFiles {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, path := range paths {
		if !safePath(path) || seen[path] {
			return ErrInvalid
		}
		seen[path] = true
	}
	return nil
}

func privateRegular(info os.FileInfo) bool {
	return info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode().Perm()&0077 == 0)
}

// ReadMirror reads exactly one bounded projection, never a directory inventory.
func ReadMirror(path string) (Mirror, error) {
	var result Mirror
	if !safePath(path) {
		return result, ErrInvalid
	}
	info, err := os.Lstat(path)
	if err != nil || !privateRegular(info) || info.Size() > MaxMirrorBytes {
		return result, ErrInvalid
	}
	file, err := os.Open(path)
	if err != nil {
		return result, ErrInvalid
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return result, ErrInvalid
	}
	body, err := io.ReadAll(io.LimitReader(file, MaxMirrorBytes+1))
	if err != nil || len(body) > MaxMirrorBytes || strictjson.Decode(body, &result) != nil || !result.Valid() {
		return Mirror{}, ErrInvalid
	}
	return result, nil
}

// WriteMirror atomically replaces only a private regular output in an existing
// private real directory. Symlinks and arbitrary existing output types refuse.
func WriteMirror(path string, value Mirror) error {
	if !safePath(path) || !value.Valid() {
		return ErrInvalid
	}
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return ErrInvalid
	}
	if current, err := os.Lstat(path); err == nil {
		if !privateRegular(current) {
			return ErrInvalid
		}
		if _, err := ReadMirror(path); err != nil {
			return ErrInvalid
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrInvalid
	}
	body, err := json.Marshal(value)
	if err != nil || len(body)+1 > MaxMirrorBytes {
		return ErrInvalid
	}
	file, err := os.CreateTemp(dir, ".backup-health-*")
	if err != nil {
		return ErrInvalid
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(append(body, '\n')); err != nil {
		return ErrInvalid
	}
	if file.Sync() != nil || file.Close() != nil {
		return ErrInvalid
	}
	// Recheck before rename; rename never follows a replaced final symlink.
	if current, err := os.Lstat(path); err == nil {
		if !privateRegular(current) {
			return ErrInvalid
		}
		if _, err := ReadMirror(path); err != nil {
			return ErrInvalid
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrInvalid
	}
	if os.Rename(file.Name(), path) != nil {
		return ErrInvalid
	}
	parent, err := os.Open(dir)
	if err != nil {
		return ErrInvalid
	}
	defer parent.Close()
	if parent.Sync() != nil {
		return ErrInvalid
	}
	return nil
}
