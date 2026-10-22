package offsite

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/backup"
)

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
