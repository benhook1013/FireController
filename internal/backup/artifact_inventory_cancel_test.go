package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// checkBundleFiles checks context once after listing the root, once before
// entering its only artifact directory, then for its first artifact.
type artifactScanCancelContext struct {
	context.Context
	checks int
}

func (c *artifactScanCancelContext) Err() error {
	c.checks++
	if c.checks >= 3 {
		return context.Canceled
	}
	return nil
}
func TestArtifactInventoryCancellationIsUnavailable(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "review-captures")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "one.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	expected := map[string]bool{"review-captures/one.json": true}
	if err := checkBundleFiles(t.Context(), root, expected); err != nil {
		t.Fatal("baseline", err)
	}
	ctx := &artifactScanCancelContext{Context: t.Context()}
	backupCode(t, checkBundleFiles(ctx, root, expected), "backup_unavailable")
	if ctx.checks != 3 {
		t.Fatalf("did not cancel at child inventory: %d", ctx.checks)
	}
}
