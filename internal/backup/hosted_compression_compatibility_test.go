package backup

// This fixture tests compatibility boundaries, not a production migration.
import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestHostedCompressionBackupRequiresCanonicalMaterialization(t *testing.T) {
	f := newRestoreFixture(t)
	_, hosted := bundleNativeReferences(t, f)
	path := filepath.Join(f.dir, hosted)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var packed bytes.Buffer
	zw := gzip.NewWriter(&packed)
	if _, err = zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, packed.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "compressed-in-place")); err == nil {
		t.Fatal("compressed canonical bytes accepted by old backup contract")
	}
	if err = os.Rename(path, path+".gz"); err != nil {
		t.Fatal(err)
	}
	if _, err = Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "compressed-only")); err == nil {
		t.Fatal("missing referenced raw capture skipped silently")
	}
	zr, err := gzip.NewReader(bytes.NewReader(packed.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := io.ReadAll(io.LimitReader(zr, int64(len(raw)+1)))
	if err != nil {
		t.Fatal(err)
	}
	zr.Close()
	if !bytes.Equal(raw, restored) {
		t.Fatal("materialization changed logical bytes")
	}
	if err = os.WriteFile(path, restored, 0600); err != nil {
		t.Fatal(err)
	}
	created, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "raw-materialized"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Inspect(t.Context(), created.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	if _, err = Restore(t.Context(), created.Path, destination); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(destination, hosted)); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("restored canonical capture changed", err)
	}
	if previous := os.Getenv("FC_CAPTURE_PREVIOUS_READER"); previous != "" {
		binary, err := os.ReadFile(previous)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprintf("%x", sha256.Sum256(binary)) != "9c47262dba4bfd29cbd75209aed6093c59fe6aab6f3cd2aa7a5146d260bc9a88" {
			t.Fatal("unqualified previous reader")
		}
		oldDestination := filepath.Join(t.TempDir(), "old-inactive")
		command := exec.CommandContext(t.Context(), "/usr/bin/unshare", "--user", "--map-root-user", "--net", previous, "restore", "--bundle", created.Path, "--data-dir", oldDestination)
		command.Env = []string{"HOME=" + t.TempDir(), "PATH=/nonexistent"}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("old reader: %v %s", err, output)
		}
		if got, err := os.ReadFile(filepath.Join(oldDestination, hosted)); err != nil || !bytes.Equal(got, raw) {
			t.Fatal("old reader changed capture", err)
		}
		t.Log("pinned old packaged reader restored exact canonical capture offline")
	}
}
