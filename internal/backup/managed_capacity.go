package backup

import (
	"context"
	"io/fs"
	"math"
	"path/filepath"
)

// Count only this instance's registered copies. Stat sizes select a capacity
// boundary; they never substitute for verified ownership and usable keepers.
func (m *Manager) retentionCount(ctx context.Context) (int, bool, error) {
	budget := m.config.MaxBytes
	if budget == 0 {
		budget = DefaultMaxBytes
	}
	rows, err := m.store.DB().QueryContext(ctx, `SELECT id FROM managed_backup_bundles WHERE destination=? AND source_instance_id=? AND state='retained' ORDER BY sequence DESC LIMIT ?`, m.config.Destination, m.store.InstanceID(), m.config.Keep)
	if err != nil {
		return 0, false, unavailable()
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) != nil {
			rows.Close()
			return 0, false, invalid()
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, false, unavailable()
	}
	var used int64
	keep := 0
	for _, id := range ids {
		if !backupID.MatchString(id) {
			return 0, false, invalid()
		}
		size, err := bundleDiskBytes(ctx, filepath.Join(m.config.Destination, id))
		if err != nil {
			return 0, false, err
		}
		if size > budget-used {
			if keep == 0 {
				return 1, true, nil
			} // Never delete the latest usable copy.
			break
		}
		used += size
		keep++
	}
	return keep, false, nil
}

func bundleDiskBytes(ctx context.Context, path string) (int64, error) {
	var total int64
	count := 0
	err := filepath.WalkDir(path, func(path string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return unavailable()
		}
		if err != nil || entry.Type()&fs.ModeSymlink != 0 {
			return invalid()
		}
		count++
		if count > maxArtifacts+10 {
			return invalid()
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > math.MaxInt64-total {
			return invalid()
		}
		total += info.Size()
		return nil
	})
	return total, err
}
