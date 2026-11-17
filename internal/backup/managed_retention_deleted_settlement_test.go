package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestManagedRetentionRecordsCompletedDeletionAfterCancellation(t *testing.T) {
	m, b, _ := retentionCancellationFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m.remove = func(ctx context.Context, path string, manifest Manifest) error {
		if err := removeManagedBundle(ctx, path, manifest); err != nil {
			return err
		}
		cancel()
		return nil
	}
	if err := m.retain(ctx); err == nil {
		t.Fatal("caller cancellation was not reported")
	}
	if _, err := os.Lstat(filepath.Join(b.destination, b.id)); !os.IsNotExist(err) {
		t.Fatalf("real removal did not finish: %v", err)
	}
	if state := retentionBundleState(t, m, b); state != "deleted" {
		t.Fatalf("completed removal left a false cleanup hold: %s", state)
	}
	if err := m.retain(t.Context()); err != nil {
		t.Fatalf("later retention remained blocked: %v", err)
	}
}
