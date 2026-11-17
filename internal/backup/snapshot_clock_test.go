package backup

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSnapshotClockUsesMonotonicElapsedForSelfContainedBundle(t *testing.T) {
	for _, wallChange := range []time.Duration{-time.Hour, time.Hour} {
		t.Run(wallChange.String(), func(t *testing.T) {
			f := newRestoreFixture(t)
			before := restoreTree(t, f.dir)
			original := activationRows(t, f.dir)
			started := time.Now() // Keep the monotonic reading in the anchor.
			elapsed := 125 * time.Millisecond
			nowCalls, sinceCalls := 0, 0
			now := func() time.Time {
				nowCalls++
				if nowCalls == 1 {
					return started
				}
				return started.Add(wallChange)
			}
			since := func(anchor time.Time) time.Duration {
				sinceCalls++
				if anchor != started {
					t.Fatal("snapshot lost its original monotonic anchor")
				}
				return elapsed
			}
			bundle, err := createWithClock(t.Context(), f.store.DB(), f.store.InstanceID(), f.dir, filepath.Join(t.TempDir(), "bundles"), now, since)
			if err != nil {
				t.Fatal("wall-clock change rejected a valid snapshot", err)
			}
			if nowCalls != 1 || sinceCalls != 1 || bundle.SnapshotStartedAt != started.UTC().Format(time.RFC3339Nano) || bundle.SnapshotFinishedAt != started.Add(elapsed).UTC().Format(time.RFC3339Nano) {
				t.Fatalf("snapshot used independent wall time: now=%d since=%d start=%s finish=%s", nowCalls, sinceCalls, bundle.SnapshotStartedAt, bundle.SnapshotFinishedAt)
			}
			// SQLite readers may update the transient WAL-index read marks.
			after := restoreTree(t, f.dir)
			delete(before, "firecontroller.sqlite-shm")
			delete(after, "firecontroller.sqlite-shm")
			if !reflect.DeepEqual(before, after) {
				t.Fatal("clock-safe creation modified durable source files")
			}
			if !reflect.DeepEqual(original, activationRows(t, f.dir)) {
				t.Fatal("clock-safe creation modified source rows")
			}
			immutable := restoreTree(t, bundle.Path)
			if err = f.store.Close(); err != nil {
				t.Fatal(err)
			}
			if err = os.Rename(f.dir, f.dir+"-offline"); err != nil {
				t.Fatal(err)
			}
			if _, err = Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal("self-contained inspection failed after source removal", err)
			}
			dest := filepath.Join(t.TempDir(), "restored")
			restored, err := Restore(t.Context(), bundle.Path, dest)
			if err != nil || restored.State != "inactive" || restored.SourceInstanceID != f.store.InstanceID() || restored.InstanceID == restored.SourceInstanceID {
				t.Fatal("clock-safe bundle could not restore independently", restored, err)
			}
			actual := activationRows(t, dest)
			for name, rows := range original {
				if name != "metadata" && name != "credentials" && !reflect.DeepEqual(rows, actual[name]) {
					t.Fatalf("restore changed retained %s rows", name)
				}
			}
			if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
				t.Fatal("inspection or restore changed immutable bundle")
			}
		})
	}
}
