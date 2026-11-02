package offsite

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOffsiteCompressedArchiveFinalizationBoundary(t *testing.T) {
	// Exercise real gzip overhead with a small deterministic incompressible
	// stream; full native pack/tar/unpack behavior has separate boundary proof.
	input := make([]byte, 1024)
	random := rand.NewChaCha8([32]byte{1})
	if _, err := random.Read(input); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"exact", "oversized", "cancelled", "closed", "private_permissions"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "private_permissions" && runtime.GOOS == "windows" {
				t.Skip("POSIX permission boundary")
			}
			path := filepath.Join(t.TempDir(), "archive.tar.gz")
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			gz := gzip.NewWriter(file)
			if _, err := gz.Write(input); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			bytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(bytes) <= len(input) {
				t.Fatal("fixture did not exercise gzip expansion", len(bytes))
			}
			limit := int64(len(bytes))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch kind {
			case "oversized":
				limit--
			case "cancelled":
				limit--
				cancel()
			case "closed":
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			case "private_permissions":
				if err := file.Chmod(0644); err != nil {
					t.Fatal(err)
				}
			}
			hash, size, err := finishPackedArchive(ctx, file, limit)
			if kind == "exact" {
				expected := sha256.Sum256(bytes)
				if err != nil || size != int64(len(bytes)) || hash != hex.EncodeToString(expected[:]) {
					t.Fatal("exact-limit archive changed", hash, size, err)
				}
				return
			}
			code := map[string]string{"oversized": "offsite_size_limit", "cancelled": "offsite_cancelled", "closed": "offsite_storage_unavailable", "private_permissions": "offsite_archive_invalid"}[kind]
			wantCode(t, err, code)
			if hash != "" || size != 0 {
				t.Fatal("refused archive returned a receipt hash", hash, size)
			}
		})
	}
}
