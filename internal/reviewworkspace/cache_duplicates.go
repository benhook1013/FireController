package reviewworkspace

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PrunePackedDuplicates removes only loose objects also present in fully
// verified local packs. It never repacks, expires refs, or prunes unreachable
// objects. Shared checkouts can therefore continue using their cache alternates.
// This maintenance primitive is not installed on a timer. A caller must use a
// low-frequency idle maintenance lane, never review preparation's latency budget.
// The private cache has one cooperating Manager writer; foreign Git writers and
// hostile same-user changes are outside this contract.
func (m *Manager) PrunePackedDuplicates(ctx context.Context, repositoryID int64) error {
	if m == nil || ctx == nil || m.fetchGate == nil {
		return errInput
	}
	cache, err := m.CachePath(repositoryID)
	if err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	select {
	case m.fetchGate <- struct{}{}:
		defer func() { <-m.fetchGate }()
	case <-bounded.Done():
		return bounded.Err()
	}
	if bounded.Err() != nil || m.layout() != nil || privateDirectory(cache, false) != nil {
		return errConfiguration
	}
	gitDir := "--git-dir=" + cache
	bare, err := m.fetchOutput(bounded, cache, "", gitDir, "rev-parse", "--is-bare-repository")
	if err != nil || bare != "true" {
		return errVerification
	}
	indexes, err := duplicateCacheInventory(bounded, cache)
	if err != nil {
		return err
	}
	if len(indexes) == 0 {
		return nil
	}
	// verify-pack checks pack checksums and every packed object's content. Merely
	// finding a hash in an index is not proof that the packed replacement is good.
	args := []string{gitDir, "verify-pack", "--"}
	args = append(args, indexes...)
	if _, err = m.fetchOutput(bounded, cache, "", args...); err != nil {
		return errVerification
	}
	if bounded.Err() != nil {
		return bounded.Err()
	}
	// prune-packed only unlinks loose duplicates; it does not delete pack files.
	// A timeout may leave partial duplicate cleanup, but never authorizes deleting
	// unique objects. Retrying revalidates every pack before continuing.
	_, err = m.fetchOutput(bounded, cache, "", gitDir, "prune-packed", "-q")
	return err
}

func duplicateCacheInventory(ctx context.Context, cache string) ([]string, error) {
	objects := filepath.Join(cache, "objects")
	safeDir := func(path string) bool {
		i, e := os.Lstat(path)
		return e == nil && i.IsDir() && i.Mode()&os.ModeSymlink == 0
	}
	if !safeDir(objects) {
		return nil, errVerification
	}
	entries, err := readDuplicateDirectory(objects, 258)
	if err != nil || len(entries) > 258 {
		return nil, errVerification
	}
	var indexes []string
	var bytes int64
	files := 0
	for _, entry := range entries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		name := entry.Name()
		if name != "info" && name != "pack" && !lowerHex(name, 2) {
			return nil, errVerification
		}
		dir := filepath.Join(objects, name)
		if !safeDir(dir) {
			return nil, errVerification
		}
		children, err := readDuplicateDirectory(dir, 65536-files)
		if err != nil || len(children) > 65536 {
			return nil, errVerification
		}
		for _, child := range children {
			if name == "info" && (child.Name() == "alternates" || child.Name() == "http-alternates") {
				return nil, errVerification
			}
			files++
			if files > 65536 || ctx.Err() != nil {
				return nil, errVerification
			}
			info, err := os.Lstat(filepath.Join(dir, child.Name()))
			if err != nil || !info.Mode().IsRegular() {
				return nil, errVerification
			}
			if name == "pack" {
				bytes += info.Size()
				if bytes > 2<<30 {
					return nil, errVerification
				}
				if strings.HasSuffix(child.Name(), ".idx") {
					stem := strings.TrimSuffix(child.Name(), ".idx")
					if !strings.HasPrefix(stem, "pack-") || !lowerHex(strings.TrimPrefix(stem, "pack-"), 40) {
						return nil, errVerification
					}
					pack, err := os.Lstat(filepath.Join(dir, stem+".pack"))
					if err != nil || !pack.Mode().IsRegular() {
						return nil, errVerification
					}
					indexes = append(indexes, filepath.Join(dir, child.Name()))
					if len(indexes) > 512 {
						return nil, errVerification
					}
				}
			} else if name != "info" && !lowerHex(child.Name(), 38) {
				return nil, errVerification
			}
		}
	}
	return indexes, nil
}

func readDuplicateDirectory(path string, limit int) ([]os.DirEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(limit + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > limit {
		return nil, errVerification
	}
	return entries, nil
}
