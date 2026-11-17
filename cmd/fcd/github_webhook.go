package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

// The operator secret is loaded only after the restore provider fence. It is
// separate from agent credentials and never enters the database or a URL.
func loadGitHubWebhookSecret(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	invalid := func() ([]byte, error) {
		return nil, core.Fail("configuration_invalid", "The GitHub webhook secret must be an absolute private regular file containing 32-4096 printable bytes.", 500)
	}
	if !filepath.IsAbs(path) {
		return invalid()
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() > 4097 || (runtime.GOOS != "windows" && before.Mode().Perm()&0077 != 0) {
		return invalid()
	}
	f, err := os.Open(path)
	if err != nil {
		return invalid()
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) || (runtime.GOOS != "windows" && opened.Mode().Perm()&0077 != 0) {
		_ = f.Close()
		return invalid()
	}
	data, err := io.ReadAll(io.LimitReader(f, 4098))
	closeErr := f.Close()
	if err != nil || closeErr != nil || len(data) > 4097 {
		return invalid()
	}
	secret := strings.TrimSuffix(string(data), "\n")
	if len(secret) < 32 || len(secret) > 4096 {
		return invalid()
	}
	for _, b := range []byte(secret) {
		if b < 0x21 || b > 0x7e {
			return invalid()
		}
	}
	return []byte(secret), nil
}
