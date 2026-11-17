package offsite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func batchFixture(t *testing.T) (*fixture, BatchConfig, []Config, []transport) {
	f := newFixture(t)
	f.bundle(t)
	root := t.TempDir()
	b := BatchConfig{Version: 1, MinFreeBytes: 1, StateDir: filepath.Join(root, "batch"), Configs: []string{filepath.Join(root, "a.json"), filepath.Join(root, "b.json"), filepath.Join(root, "c.json")}}
	cs := []Config{f.c, f.c, f.c}
	ts := []transport{memory(), memory(), memory()}
	for i := range cs {
		cs[i].StateDir = filepath.Join(root, string(rune('a'+i)))
		cs[i].Remote = "cloud:" + string(rune('a'+i))
	}
	return f, b, cs, ts
}
func TestBatchSharedArchiveKeepsIndependentVerification(t *testing.T) {
	_, b, cs, ts := batchFixture(t)
	ts[1].(*memoryTransport).corruptDownload = true
	got, err := pushBatch(context.Background(), b, cs, ts)
	if err == nil || len(got.Destinations) != 3 || got.Destinations[0].Reason != "" || got.Destinations[1].Reason == "" || got.Destinations[2].Reason != "" {
		t.Fatalf("%+v %v", got, err)
	}
	a, _ := readState(cs[0])
	z, _ := readState(cs[2])
	pending, _ := readState(cs[1])
	if a.LastSuccess == nil || z.LastSuccess == nil || *a.LastSuccess != *z.LastSuccess || pending.Pending == nil || pending.LastSuccess != nil {
		t.Fatal("destination independence lost")
	}
	// Batch cleanup must not destroy a failed provider's hardlinked spool.
	if _, err := os.Stat(filepath.Join(cs[1].StateDir, "pending.tar.gz")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.StateDir, "work")); !os.IsNotExist(err) {
		t.Fatal("batch workspace retained", err)
	}
	ts[1].(*memoryTransport).corruptDownload = false
	if err := os.RemoveAll(cs[1].SourceRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := push(context.Background(), cs[1], ts[1]); err != nil {
		t.Fatal("pending cannot resume without source", err)
	}
}
func TestBatchHardlinkReferenceIsolation(t *testing.T) {
	f := newFixture(t)
	s := f.bundle(t)
	work := t.TempDir()
	p := &batchPreparation{work: work}
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	r, err := p.linkArchive(t.Context(), s, a, f.c)
	if err != nil {
		t.Fatal(err)
	}
	// Source removal demonstrates preparation is reused, not packed again.
	if err := os.RemoveAll(s.Path); err != nil {
		t.Fatal(err)
	}
	rr, err := p.linkArchive(t.Context(), s, b, f.c)
	if err != nil || r != rr {
		t.Fatal(err)
	}
	ai, _ := os.Stat(a)
	bi, _ := os.Stat(b)
	if !os.SameFile(ai, bi) {
		t.Fatal("archive copied instead of linked")
	}
	os.Remove(a)
	os.RemoveAll(work)
	h, n, err := fileHash(t.Context(), b, f.c.MaxBundleBytes)
	if err != nil || h != r.ArchiveSHA256 || n != r.ArchiveBytes {
		t.Fatal("reference cleanup removed pending", err)
	}
}
func TestBatchRejectsOverlapsAndDuplicateDestinations(t *testing.T) {
	for _, variant := range []string{"source", "state", "duplicate", "limit"} {
		t.Run(variant, func(t *testing.T) {
			_, b, cs, ts := batchFixture(t)
			switch variant {
			case "source":
				b.StateDir = cs[0].SourceRoot
			case "state":
				b.StateDir = cs[0].StateDir
			case "duplicate":
				cs[1].Remote = cs[0].Remote
			case "limit":
				cs[1].MaxBundleBytes++
			}
			if _, err := pushBatch(t.Context(), b, cs, ts); err == nil {
				t.Fatal("unsafe batch admitted")
			}
			for _, tr := range ts {
				if len(tr.(*memoryTransport).puts) != 0 {
					t.Fatal("remote effect before validation")
				}
			}
		})
	}
}

func TestBatchHeadroomRefusesBeforeRemoteEffects(t *testing.T) {
	_, b, cs, ts := batchFixture(t)
	b.MinFreeBytes = maxBundleBytes
	_, err := pushBatch(t.Context(), b, cs, ts)
	wantCode(t, err, "offsite_storage_headroom")
	for _, tr := range ts {
		if len(tr.(*memoryTransport).puts) != 0 {
			t.Fatal("headroom refusal uploaded")
		}
	}
}
func TestBatchBusyDoesNotReclaimActiveWorkspace(t *testing.T) {
	_, b, cs, ts := batchFixture(t)
	if err := privateDir(b.StateDir, true); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(b.StateDir, "batch.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := lock(f); err != nil {
		t.Fatal(err)
	}
	_, err = pushBatch(t.Context(), b, cs, ts)
	wantCode(t, err, "offsite_busy")
}
func TestBatchRejectsCredentialAndExecutableOverlap(t *testing.T) {
	for _, field := range []string{"credential", "executable"} {
		t.Run(field, func(t *testing.T) {
			_, b, cs, ts := batchFixture(t)
			p := filepath.Join(b.StateDir, "work", "valuable")
			if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			if field == "credential" {
				cs[1].RcloneConfig = p
			} else {
				cs[1].RcloneBinary = p
			}
			_, err := pushBatch(t.Context(), b, cs, ts)
			wantCode(t, err, "offsite_configuration_invalid")
			if got, err := os.ReadFile(p); err != nil || string(got) != "keep" {
				t.Fatal("protected input removed", err)
			}
		})
	}
}

func TestBatchRejectsAllCrossProviderProtectedInputs(t *testing.T) {
	for _, kind := range []string{"provider-config", "credential", "executable", "batch-config"} {
		t.Run(kind, func(t *testing.T) {
			_, b, cs, ts := batchFixture(t)
			if err := privateDir(cs[0].StateDir, true); err != nil {
				t.Fatal(err)
			}
			work, err := prepareWork(cs[0])
			if err != nil {
				t.Fatal(err)
			}
			protected := filepath.Join(work, "protected-input")
			if err := os.WriteFile(protected, []byte("preserve exactly"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "provider-config":
				b.Configs[1] = protected
			case "credential":
				cs[1].RcloneConfig = protected
			case "executable":
				cs[2].RcloneBinary = protected
			case "batch-config":
				b.configPath = protected
			}
			// The workspace has a fully valid marker: without the cross-product guard,
			// first provider startup would recursively reclaim the protected input.
			_, err = pushBatch(t.Context(), b, cs, ts)
			wantCode(t, err, "offsite_configuration_invalid")
			got, err := os.ReadFile(protected)
			if err != nil || string(got) != "preserve exactly" {
				t.Fatal("cross-provider input removed", err)
			}
			if _, err := os.Stat(filepath.Join(work, ".owner")); err != nil {
				t.Fatal("owned workspace changed", err)
			}
			for _, tr := range ts {
				if len(tr.(*memoryTransport).puts) != 0 {
					t.Fatal("provider effect before validation")
				}
			}
		})
	}
}
