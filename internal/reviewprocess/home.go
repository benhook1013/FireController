package reviewprocess

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Login state the CLI keeps under HOME. Each run gets private copies, and a
// changed copy (a refreshed login) is written back atomically.
var sharedHomeFiles = []string{".coderabbit/auth.json", ".coderabbit/machine-id"}

const maxHomeFile = 1 << 20

// WithIsolatedHomes gives every CLI run its own HOME and XDG directories under
// root, so parallel runs never share caches, logs or statistics. Only the login
// files are copied from the configured HOME. Leftover run homes are removed.
func WithIsolatedHomes(root string) Option {
	return func(r *Runner) error {
		if !filepath.IsAbs(root) || r.home == "" {
			return processError("configuration_invalid", false)
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			return processError("configuration_invalid", false)
		}
		if err := os.Chmod(root, 0o700); err != nil {
			return processError("configuration_invalid", false)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return processError("configuration_invalid", false)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "run-") {
				_ = os.RemoveAll(filepath.Join(root, entry.Name()))
			}
		}
		r.homeRoot = root
		return nil
	}
}

type runHome struct {
	dir    string
	shared string
	copied map[string][]byte
}

// prepareHome returns the child environment for one run and its release.
// Release keeps a changed login only after a clean exit.
func (r *Runner) prepareHome() ([]string, func(clean bool), error) {
	env := append([]string(nil), r.env...)
	if r.homeRoot == "" {
		return env, func(bool) {}, nil
	}
	suffix := make([]byte, 12)
	if _, err := rand.Read(suffix); err != nil {
		return nil, nil, err
	}
	h := runHome{dir: filepath.Join(r.homeRoot, "run-"+hex.EncodeToString(suffix)), shared: r.home, copied: map[string][]byte{}}
	if err := os.Mkdir(h.dir, 0o700); err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(h.dir) }
	for _, name := range sharedHomeFiles {
		// The shared login may be a mounted symlink; read its target.
		data, err := readShared(filepath.Join(h.shared, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err == nil {
			err = writeAtomic(filepath.Join(h.dir, name), data)
		}
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		h.copied[name] = data
	}
	overrides := map[string]string{"HOME": h.dir, "XDG_CACHE_HOME": filepath.Join(h.dir, ".cache"), "XDG_CONFIG_HOME": filepath.Join(h.dir, ".config"), "XDG_DATA_HOME": filepath.Join(h.dir, ".local", "share")}
	out := env[:0]
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if _, ok := overrides[key]; !ok {
			out = append(out, entry)
		}
	}
	for key, value := range overrides {
		out = append(out, key+"="+value)
	}
	return out, func(clean bool) {
		defer cleanup()
		if !clean {
			return
		}
		// Keep a login the CLI refreshed during this run, but only when no
		// other run changed the shared copy meanwhile. Never delete one.
		r.homeMu.Lock()
		defer r.homeMu.Unlock()
		for _, name := range sharedHomeFiles {
			data, err := readSmall(filepath.Join(h.dir, name))
			if err != nil || len(data) == 0 || bytes.Equal(data, h.copied[name]) || strings.HasSuffix(name, ".json") && !json.Valid(data) {
				continue
			}
			target := filepath.Join(h.shared, name)
			if current, err := readShared(target); err != nil || !bytes.Equal(current, h.copied[name]) {
				continue
			}
			_ = writeAtomic(target, data)
		}
	}, nil
}

func readShared(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxHomeFile {
		return nil, errors.New("home file is not a small regular file")
	}
	return os.ReadFile(path)
}

func readSmall(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxHomeFile {
		return nil, errors.New("home file is not a small regular file")
	}
	return os.ReadFile(path)
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".fc-home-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	_, writeErr := temp.Write(data)
	syncErr := temp.Sync()
	closeErr := temp.Close()
	if err := errors.Join(writeErr, syncErr, closeErr, os.Chmod(name, 0o600)); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
