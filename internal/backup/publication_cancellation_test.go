package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Like restoreMarkerCancellation, this context samples an observable filesystem
// boundary instead of adding a production hook or depending on a timing race.
// Publication is the cancellation boundary: a context check must not undo it.
type bundlePublicationCancellation struct {
	context.Context
	cancel      context.CancelFunc
	destination string
}

func (c bundlePublicationCancellation) Err() error {
	entries, _ := os.ReadDir(c.destination)
	for _, entry := range entries {
		if entry.IsDir() && backupID.MatchString(entry.Name()) {
			c.cancel()
			break
		}
	}
	return c.Context.Err()
}

func TestBundleCancellationAtPublicationPreservesCompleteBundle(t *testing.T) {
	f := newRestoreFixture(t)
	destination := filepath.Join(t.TempDir(), "bundles")
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := bundlePublicationCancellation{Context: base, cancel: cancel, destination: destination}
	bundle, err := Create(ctx, f.store, f.dir, destination)
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("fixture did not reach the published-bundle cancellation boundary")
	}
	if err != nil || bundle.State != "complete" {
		t.Fatal("publication was rolled back by a late context check", bundle, err)
	}
	verified, err := Inspect(t.Context(), bundle.Path)
	if err != nil || verified.ID != bundle.ID || verified.SourceInstanceID != f.store.InstanceID() {
		t.Fatal("published complete bundle was removed or became unusable", verified, err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil || len(entries) != 1 || entries[0].Name() != bundle.ID {
		t.Fatal("publication left staging or lost its final bundle", entries, err)
	}
}
