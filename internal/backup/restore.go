package backup

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

const restoreMarker = ".restore-in-progress"

// RestoreSummary describes a local inactive copy. It grants no serving or
// provider execution authority and does not relabel retained receipts/intents.
type RestoreSummary struct {
	BundleID         string `json:"bundle_id"`
	SourceInstanceID string `json:"source_instance_id"`
	InstanceID       string `json:"instance_id"`
	State            string `json:"state"`
	Destination      string `json:"destination"`
	EventSequence    int64  `json:"event_sequence"`
	ArtifactCount    int64  `json:"artifact_count"`
}

func inactiveRestore() error {
	return core.Fail("restore_inactive", "The restored installation is inactive; activation and external reconciliation are required.", 503)
}

// CheckDataDirectory must run before core.Open, which can create a fresh DB.
// A dangling or otherwise unreadable marker also blocks startup.
func CheckDataDirectory(dataDir string) error {
	if dataDir == "" {
		return invalid()
	}
	_, err := os.Lstat(filepath.Join(dataDir, restoreMarker))
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return inactiveRestore()
	}
	return nil
}

// StartupGuard must precede listeners and workers. An ordinary installation has
// no restore metadata. Restored copies cannot serve, even with unknown state.
func StartupGuard(ctx context.Context, store *core.Store) error {
	if ctx == nil || store == nil || ctx.Err() != nil {
		return unavailable()
	}
	var count int64
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM metadata WHERE key GLOB 'restore:*'`).Scan(&count); err != nil {
		return unavailable()
	}
	if count != 0 {
		return inactiveRestore()
	}
	return nil
}

// Restore creates a new private destination and retains the incomplete marker
// until the copied DB, references, artifacts, identity and revocation are durable.
// Activation is intentionally absent: old receipts and provider intents retain
// their predecessor provenance and must never be resumed by this function.
func Restore(ctx context.Context, bundleDir, destination string) (RestoreSummary, error) {
	var zero RestoreSummary
	if err := checkContext(ctx); err != nil {
		return zero, err
	}
	m, err := inspectManifest(ctx, bundleDir)
	if err != nil {
		return zero, err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return zero, invalid()
	}
	bundleAbsolute, e := filepath.Abs(bundleDir)
	if e != nil || within(destination, bundleAbsolute) || within(bundleAbsolute, destination) {
		return zero, invalid()
	}
	if destination == "" || len(destination) > 3000 {
		return zero, invalid()
	}
	// Never overlay an existing path, including an empty directory or a symlink.
	if _, err = os.Lstat(destination); err == nil {
		return zero, exists()
	} else if !errors.Is(err, os.ErrNotExist) {
		return zero, unavailable()
	}
	if err = restoreParent(filepath.Dir(destination)); err != nil {
		return zero, err
	}
	if err = os.Mkdir(destination, 0700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return zero, exists()
		}
		return zero, unavailable()
	}
	owned, err := os.Lstat(destination)
	if err != nil {
		return zero, unavailable()
	}
	complete := false
	defer func() {
		if !complete {
			if current, e := os.Lstat(destination); e == nil && os.SameFile(owned, current) && current.IsDir() {
				_ = os.RemoveAll(destination)
			}
		}
	}()
	marker, err := os.OpenFile(filepath.Join(destination, restoreMarker), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return zero, unavailable()
	}
	if _, err = marker.WriteString("inactive restore incomplete\n"); err == nil {
		err = marker.Sync()
	}
	err = errors.Join(err, marker.Close())
	if err != nil {
		return zero, unavailable()
	}
	if err = syncDirectory(destination); err != nil {
		return zero, err
	}
	if err = syncDirectory(filepath.Dir(destination)); err != nil {
		return zero, err
	}
	copyOne := func(a Artifact, limit int64) error {
		if err := checkContext(ctx); err != nil {
			return err
		}
		target := filepath.Join(destination, filepath.FromSlash(a.Path))
		if err := privateDirectory(filepath.Dir(target), true); err != nil {
			return err
		}
		copied, err := copyPrivate(ctx, filepath.Join(bundleDir, filepath.FromSlash(a.Path)), target, limit)
		if err != nil {
			return err
		}
		if copied.SHA256 != a.SHA256 || copied.Bytes != a.Bytes {
			return invalid()
		}
		return nil
	}
	if err = copyOne(m.Database, maxDatabaseBytes); err != nil {
		return zero, err
	}
	for _, a := range m.Artifacts {
		if err = copyOne(a, maxHostedCaptureBytes); err != nil {
			return zero, err
		}
	}
	if err = validateSnapshot(ctx, destination, m); err != nil {
		return zero, err
	}
	id := core.NewID("instance")
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return zero, unavailable()
	}
	if err = restoreIdentity(ctx, destination, m, id, hex.EncodeToString(key)); err != nil {
		return zero, err
	}
	restored := m
	restored.SourceInstanceID = id
	if err = verifyRestoredIdentity(ctx, destination, m, id, hex.EncodeToString(key)); err != nil {
		return zero, err
	}
	if err = validateSnapshotContent(ctx, destination, restored, true); err != nil {
		return zero, err
	}
	// Sync every owned artifact directory, deepest first, before publishing the
	// absence of the startup marker. No symlink traversal is needed here.
	if err = syncRestoreDirectories(ctx, destination); err != nil {
		return zero, err
	}
	summary := RestoreSummary{BundleID: m.ID, SourceInstanceID: m.SourceInstanceID, InstanceID: id, State: "inactive", Destination: destination, EventSequence: m.EventSequence, ArtifactCount: int64(len(m.Artifacts))}
	encoded, err := json.Marshal(summary)
	if err != nil || len(encoded)+1 > 4096 {
		return zero, invalid()
	}
	if err = checkContext(ctx); err != nil {
		return zero, err
	}
	if err = os.Remove(filepath.Join(destination, restoreMarker)); err != nil {
		return zero, unavailable()
	}
	if err = syncDirectory(destination); err != nil {
		return zero, err
	}
	if err = syncDirectory(filepath.Dir(destination)); err != nil {
		return zero, err
	}
	complete = true
	return summary, nil
}

func restoreIdentity(ctx context.Context, dir string, m Manifest, id, key string) error {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(dir, "firecontroller.sqlite"))}
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path
	}
	u.RawQuery = url.Values{"mode": {"rw"}, "_pragma": {"foreign_keys(1)", "synchronous(FULL)", "journal_mode(DELETE)", "busy_timeout(0)"}}.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return unavailable()
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return unavailable()
	}
	defer tx.Rollback()
	values := restoreMetadata(m, id, key)
	for name, value := range values {
		if _, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, name, value); err != nil {
			return unavailable()
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE credentials SET revoked=1`); err != nil {
		return unavailable()
	}
	if err = tx.Commit(); err != nil {
		return unavailable()
	}
	if err = db.Close(); err != nil {
		return unavailable()
	}
	return nil
}
func restoreMetadata(m Manifest, id, key string) map[string]string {
	return map[string]string{"instance_id": id, "signing_key": key, "restore:state": "inactive", "restore:source_instance_id": m.SourceInstanceID, "restore:bundle_id": m.ID, "restore:snapshot_started_at": m.SnapshotStartedAt, "restore:snapshot_finished_at": m.SnapshotFinishedAt, "restore:event_sequence": strconv.FormatInt(m.EventSequence, 10)}
}
func verifyRestoredIdentity(ctx context.Context, dir string, m Manifest, id, key string) error {
	db, err := openSnapshot(ctx, filepath.Join(dir, "firecontroller.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	for name, want := range restoreMetadata(m, id, key) {
		var got string
		if err = db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, name).Scan(&got); err != nil || got != want {
			return unavailable()
		}
	}
	var active int64
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM credentials WHERE revoked<>1`).Scan(&active); err != nil || active != 0 {
		return unavailable()
	}
	return nil
}

func syncRestoreDirectories(ctx context.Context, dir string) error {
	var dirs []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return unavailable()
		}
		if err = checkContext(ctx); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return invalid()
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err = syncDirectory(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

// The newly created destination must be private. Its existing parent may be
// public (for example /tmp), but no ancestor may redirect through a symlink.
func restoreParent(path string) error {
	for {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return invalid()
		}
		next := filepath.Dir(path)
		if next == path {
			return nil
		}
		path = next
	}
}
