package offsite

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/backup"
)

func TestOffsiteArchiveActualTarSizeBoundaryIncludesPaddingAndPAX(t *testing.T) {
	f := newFixture(t)
	for _, dir := range []string{"review-captures", "hosted-review-captures"} {
		if err := os.Mkdir(filepath.Join(f.dir, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for i, size := range []int{1, 513, 1025} {
		name := fmt.Sprintf("review_attempt_%032x.json", i+1)
		if err := os.WriteFile(filepath.Join(f.dir, "review-captures", name), bytes.Repeat([]byte("x"), size), 0600); err != nil {
			t.Fatal(err)
		}
	}
	contents := []byte("synthetic forensic capture")
	digest := sha256.Sum256(contents)
	longName := "review_attempt_" + strings.Repeat("a", 32) + "-hosted-" + hex.EncodeToString(digest[:]) + ".json"
	if err := os.WriteFile(filepath.Join(f.dir, "hosted-review-captures", longName), contents, 0600); err != nil {
		t.Fatal(err)
	}
	summary := f.bundle(t)
	reference := filepath.Join(t.TempDir(), "reference.tar.gz")
	if _, err := pack(t.Context(), summary.Path, reference, f.c); err != nil {
		t.Fatal(err)
	}
	compressed, err := os.Open(reference)
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	var estimated int64
	pax := false
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		estimated += header.Size + 512
		pax = pax || strings.HasSuffix(header.Name, longName) && header.PAXRecords["path"] == header.Name
	}
	actual := int64(len(raw))
	if !pax || actual <= estimated+1024 || actual%512 != 0 {
		t.Fatal("fixture did not exercise real PAX metadata and tar padding", actual, estimated, pax)
	}
	exact := f.c
	exact.MaxBundleBytes = actual
	archive := filepath.Join(t.TempDir(), "exact.tar.gz")
	receipt, err := pack(t.Context(), summary.Path, archive, exact)
	if err != nil {
		t.Fatal("exact expanded tar limit refused a valid bundle", err)
	}
	restored, err := unpack(t.Context(), archive, t.TempDir(), receipt, exact)
	if err != nil || restored.ID != summary.ID || restored.ArtifactCount != 4 {
		t.Fatal("pack accepted a bundle its own unpack cannot recover", restored, err)
	}
	// Different failure points cover file copying, a later PAX header, and the
	// final tar end marker. Every rejected partial archive must be removed.
	firstPadded := 512 + (summary.DatabaseBytes+511)/512*512
	for _, test := range []struct {
		name  string
		limit int64
	}{
		{"file copy", 512 + summary.DatabaseBytes - 1},
		{"PAX header", firstPadded + 100},
		{"one byte below actual", actual - 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			bounded := f.c
			bounded.MaxBundleBytes = test.limit
			destination := filepath.Join(t.TempDir(), "rejected.tar.gz")
			_, err := pack(t.Context(), summary.Path, destination, bounded)
			wantCode(t, err, "offsite_size_limit")
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatal("size refusal retained an incomplete archive", err)
			}
		})
	}
}

func TestOffsiteArchiveRefusesTraversalLinksDuplicatesAndExpansion(t *testing.T) {
	f := newFixture(t)
	summary := f.bundle(t)
	for _, kind := range []string{"traversal", "absolute", "symlink", "hardlink", "duplicate", "expansion", "trailer"} {
		t.Run(kind, func(t *testing.T) {
			var b bytes.Buffer
			gz := gzip.NewWriter(&b)
			tw := tar.NewWriter(gz)
			name := summary.ID + "/manifest.json"
			header := &tar.Header{Name: name, Mode: 0600, Typeflag: tar.TypeReg, Size: 1}
			switch kind {
			case "traversal":
				header.Name = summary.ID + "/../../escaped"
			case "absolute":
				header.Name = "/escaped"
			case "symlink":
				header.Typeflag = tar.TypeSymlink
				header.Size = 0
				header.Linkname = "../../escaped"
			case "hardlink":
				header.Typeflag = tar.TypeLink
				header.Size = 0
				header.Linkname = "../../escaped"
			case "expansion":
				header.Size = f.c.MaxBundleBytes + 1
			}
			if err := tw.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if header.Typeflag == tar.TypeReg {
				var contents []byte
				if kind == "expansion" {
					contents = make([]byte, header.Size)
				} else {
					contents = []byte("x")
				}
				if _, err := tw.Write(contents); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "duplicate" {
				if tw.WriteHeader(header) != nil {
					t.Fatal("header")
				}
				tw.Write([]byte("x"))
			}
			tw.Close()
			gz.Close()
			if kind == "trailer" {
				data := b.Bytes()
				data[len(data)-1] ^= 0xff
			}
			archive := filepath.Join(t.TempDir(), "archive.gz")
			if err := os.WriteFile(archive, b.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			h := sha256.Sum256(b.Bytes())
			r := Receipt{Version: 1, ID: summary.ID, InstanceID: f.c.InstanceID, SnapshotFinishedAt: summary.SnapshotFinishedAt, EventSequence: summary.EventSequence, ArchiveSHA256: hex.EncodeToString(h[:]), ArchiveBytes: int64(b.Len())}
			_, err := unpack(t.Context(), archive, t.TempDir(), r, f.c)
			wantCode(t, err, "offsite_archive_invalid")
		})
	}
}

func TestOffsiteConfigRequiresCanonicalRemoteDestination(t *testing.T) {
	f := newFixture(t)
	for _, remote := range []string{"cloud:backups", "cloud:bucket/backups", "cloud:nested/path-1/backups_2", "cloud:backups/", "cloud:bucket/backups/", "cloud:bucket//backups"} {
		t.Run(remote, func(t *testing.T) {
			c := f.c
			c.Remote = remote
			file := filepath.Join(t.TempDir(), "config.json")
			if err := writeJSON(file, c); err != nil {
				t.Fatal(err)
			}
			got, err := loadConfig(file)
			if strings.HasSuffix(remote, "/") || strings.Contains(remote, "//") {
				wantCode(t, err, "offsite_configuration_invalid")
			} else if err != nil || got.Remote != remote {
				t.Fatal("canonical destination was rejected or normalized", got.Remote, err)
			}
		})
	}
}

func TestOffsiteConfigRefusesAliasedSourceContainmentAndStateRebinding(t *testing.T) {
	f := newFixture(t)
	f.bundle(t)
	m := memory()
	if _, err := push(t.Context(), f.c, m); err != nil {
		t.Fatal(err)
	}
	c := f.c
	c.Remote = "other:backups"
	_, err := push(t.Context(), c, m)
	wantCode(t, err, "offsite_state_invalid")
	if runtime.GOOS == "windows" {
		return
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err = os.Symlink(f.c.SourceRoot, alias); err != nil {
		t.Fatal(err)
	}
	c = f.c
	c.StateDir = filepath.Join(alias, "new", "nested", "state")
	file := filepath.Join(t.TempDir(), "config.json")
	if err = writeJSON(file, c); err != nil {
		t.Fatal(err)
	}
	_, err = loadConfig(file)
	wantCode(t, err, "offsite_configuration_invalid")
}

func TestOffsiteLatestSourceUsesEventWatermarkAcrossClockRollback(t *testing.T) {
	f := newFixture(t)
	first := f.bundle(t)
	// Give the existing copy a synthetic future snapshot time while retaining
	// a fully validated canonical manifest, sidecar and original DB reference.
	manifestPath := filepath.Join(first.Path, "manifest.json")
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(b), first.SnapshotFinishedAt, "2099-01-01T00:00:00Z")
	if err = os.WriteFile(manifestPath, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(text))
	if err = os.WriteFile(filepath.Join(first.Path, "manifest.sha256"), []byte(hex.EncodeToString(h[:])+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = backup.Inspect(t.Context(), first.Path); err != nil {
		t.Fatal(err)
	}
	latest := f.bundle(t)
	selected, err := newest(t.Context(), f.c)
	if err != nil || selected.ID != latest.ID {
		t.Fatal("selected stale snapshot by wall clock", selected, err)
	}
}
