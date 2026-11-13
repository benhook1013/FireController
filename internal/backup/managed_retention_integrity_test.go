package backup

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func retainedIntegrityBundles(t *testing.T, m *Manager) []ownedBundle {
	t.Helper()
	rows, err := m.store.DB().Query(`SELECT id,destination,source_instance_id,manifest_sha256,state FROM managed_backup_bundles WHERE destination=? AND source_instance_id=? AND state='retained' ORDER BY sequence DESC`, m.config.Destination, m.store.InstanceID())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var bundles []ownedBundle
	for rows.Next() {
		var b ownedBundle
		if err := rows.Scan(&b.id, &b.destination, &b.instance, &b.hash, &b.state); err != nil {
			t.Fatal(err)
		}
		bundles = append(bundles, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return bundles
}

func semanticValidationContext(t *testing.T) context.Context {
	t.Helper()
	base, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	return &retentionBoundaryContext{Context: base, cancel: cancel, boundary: func() bool {
		var callers [32]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		for {
			frame, more := frames.Next()
			if frame.Function == "github.com/benhook1013/FireController/internal/backup.validateSnapshotContent" {
				return true
			}
			if !more {
				return false
			}
		}
	}}
}

func TestManagedRetentionDoesNotRepeatRegisteredSemanticValidation(t *testing.T) {
	_, m := newManagedFixture(t, 3)
	if reports := runManaged(t, m, 3); len(reports) != 0 {
		t.Fatal(reports)
	}
	bundles := retainedIntegrityBundles(t, m)
	cfg := m.config
	cfg.Keep = 2
	var err error
	m, err = NewManager(m.store, m.dataDir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Deterministically exhaust the caller only if a redundant semantic SQL
	// scan is entered. Real hashing, ownership checks and removal still run.
	ctx := semanticValidationContext(t)
	if err := m.retain(ctx); err != nil || ctx.Err() != nil {
		t.Fatal("pruning repeated registered snapshot semantics", err, ctx.Err())
	}
	if _, err := os.Lstat(filepath.Join(bundles[2].destination, bundles[2].id)); !os.IsNotExist(err) {
		t.Fatal("excess verified bundle was not removed", err)
	}
	for _, b := range bundles[:2] {
		path := filepath.Join(b.destination, b.id)
		if _, err := Inspect(t.Context(), path); err != nil {
			t.Fatal("protected backup became invalid", err)
		}
		if _, err := Inspect(semanticValidationContext(t), path); err == nil {
			t.Fatal("manual inspection skipped current semantic validation")
		}
	}
}

func TestManagedRetentionChangedBytesCannotReplaceRegisteredBundle(t *testing.T) {
	for _, damage := range []string{"resealed semantic", "database bytes", "artifact bytes", "unexpected file"} {
		for _, position := range []string{"protected keeper", "deletion candidate"} {
			t.Run(damage+"/"+position, func(t *testing.T) {
				_, m := newManagedFixture(t, 3)
				if reports := runManaged(t, m, 3); len(reports) != 0 {
					t.Fatal(reports)
				}
				bundles := retainedIntegrityBundles(t, m)
				selected := bundles[1]
				if position == "deletion candidate" {
					selected = bundles[2]
				}
				path := filepath.Join(selected.destination, selected.id)
				manifest, err := inspectBundleIntegrity(t.Context(), path)
				if err != nil || len(manifest.Artifacts) == 0 {
					t.Fatal("valid source fixture unavailable", err)
				}
				switch damage {
				case "resealed semantic":
					mutateBundleSQL(t, path, `UPDATE metadata SET value=? WHERE key='instance_id'`, core.NewID("instance"))
				case "database bytes", "artifact bytes":
					file := filepath.Join(path, "firecontroller.sqlite")
					if damage == "artifact bytes" {
						file = filepath.Join(path, filepath.FromSlash(manifest.Artifacts[0].Path))
					}
					f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
					if err != nil {
						t.Fatal(err)
					}
					_, writeErr := f.Write([]byte(" "))
					closeErr := f.Close()
					if writeErr != nil || closeErr != nil {
						t.Fatal(writeErr, closeErr)
					}
				case "unexpected file":
					if err := os.WriteFile(filepath.Join(path, "operator-note"), []byte("preserve"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				manifest, err = inspectBundleIntegrity(t.Context(), path)
				if damage == "resealed semantic" {
					if err != nil {
						t.Fatal("resealed fixture must have valid physical bytes", err)
					}
					hash, err := manifestHash(manifest)
					if err != nil || hash == selected.hash {
						t.Fatal("resealing did not change the registered manifest", err)
					}
				} else if err == nil {
					t.Fatal("byte or layout damage passed physical verification")
				}
				_, err = Inspect(t.Context(), path)
				backupCode(t, err, "backup_invalid")
				destination := filepath.Join(t.TempDir(), "must-not-restore")
				_, err = Restore(t.Context(), path, destination)
				backupCode(t, err, "backup_invalid")
				if _, err := os.Lstat(destination); !os.IsNotExist(err) {
					t.Fatal("invalid source created a restore destination", err)
				}
				before := restoreTree(t, m.config.Destination)
				cfg := m.config
				cfg.Keep = 2
				m, err = NewManager(m.store, m.dataDir, cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := m.retain(t.Context()); err == nil {
					t.Fatal("changed bytes gained managed ownership")
				}
				if !reflect.DeepEqual(before, restoreTree(t, m.config.Destination)) {
					t.Fatal("refused fixture was changed")
				}
				var deleted int
				if err := m.store.DB().QueryRow(`SELECT count(*) FROM managed_backup_bundles WHERE state='deleted'`).Scan(&deleted); err != nil || deleted != 0 {
					t.Fatal("damaged keeper or candidate permitted pruning", deleted, err)
				}
				if _, err := Inspect(t.Context(), filepath.Join(bundles[0].destination, bundles[0].id)); err != nil {
					t.Fatal("newest verified backup was lost", err)
				}
			})
		}
	}
}

func TestManagedRetentionIntegrityBoundedWorkload(t *testing.T) {
	const keep, excess = 8, 2
	_, m := newManagedFixture(t, keep+excess)
	if reports := runManaged(t, m, keep+excess); len(reports) != 0 {
		t.Fatal(reports)
	}
	bundles := retainedIntegrityBundles(t, m)
	var totalBytes int64
	var artifacts int
	fullStarted := time.Now()
	for _, b := range bundles[:keep] {
		manifest, err := inspectManifest(t.Context(), filepath.Join(b.destination, b.id))
		if err != nil {
			t.Fatal(err)
		}
		totalBytes += manifest.Database.Bytes
		artifacts += len(manifest.Artifacts)
		for _, a := range manifest.Artifacts {
			totalBytes += a.Bytes
		}
	}
	fullElapsed := time.Since(fullStarted)
	cfg := m.config
	cfg.Keep = keep
	var err error
	m, err = NewManager(m.store, m.dataDir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := m.retain(t.Context()); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if got := len(retainedIntegrityBundles(t, m)); got != keep {
		t.Fatal("bounded workload did not converge", got)
	}
	for _, b := range bundles[keep:] {
		if _, err := os.Lstat(filepath.Join(b.destination, b.id)); !os.IsNotExist(err) {
			t.Fatal("excess bundle remains", err)
		}
	}
	t.Logf("synthetic keep=%d excess=%d keeper_payload_bytes=%d keeper_artifacts=%d full_keeper_inspection=%s integrity_pruning=%s", keep, excess, totalBytes, artifacts, fullElapsed, elapsed)
}
