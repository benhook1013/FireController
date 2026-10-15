package core

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
)

// Snapshot writes a self-contained SQLite snapshot through the read pool. WAL
// writers remain independent; copying the open database file is never used.
// The caller owns the private staging directory and the operation deadline.
func (s *Store) Snapshot(ctx context.Context, destination string) (err error) {
	if ctx == nil || s == nil || s.db == nil || !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return Fail("configuration_invalid", "A snapshot requires an absolute destination in a private staging directory.", 400)
	}
	if ctx.Err() != nil {
		return snapshotUnavailable()
	}
	parent := filepath.Dir(destination)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return Fail("configuration_invalid", "The snapshot staging directory is unavailable or unsafe.", 400)
	}
	file, err := os.OpenFile(destination, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			return Fail("backup_exists", "The snapshot destination already exists.", 409)
		}
		return snapshotUnavailable()
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			// Only this call's exclusively created staging file is disposable.
			_ = os.Remove(destination)
		}
	}()
	if _, err = s.db.ExecContext(ctx, "VACUUM INTO ?", destination); err != nil {
		return snapshotUnavailable()
	}
	reserved, err := file.Stat()
	current, statErr := os.Lstat(destination)
	if err != nil || statErr != nil || !current.Mode().IsRegular() || !os.SameFile(reserved, current) || current.Size() == 0 || ctx.Err() != nil {
		return snapshotUnavailable()
	}
	if err = file.Sync(); err != nil {
		return snapshotUnavailable()
	}
	if err = file.Close(); err != nil {
		return snapshotUnavailable()
	}
	// Directory fsync is not supported by ordinary Windows directory handles.
	// Other platforms must durably publish the new staging entry before use.
	if runtime.GOOS != "windows" {
		directory, openErr := os.Open(parent)
		if openErr != nil {
			return snapshotUnavailable()
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil || closeErr != nil {
			return snapshotUnavailable()
		}
	}
	complete = true
	return nil
}

func snapshotUnavailable() error {
	return Fail("backup_unavailable", "The database snapshot could not be completed.", 503)
}
