package offsite

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestOffsiteLowerTransferLimitPreservesRetainedReceipts(t *testing.T) {
	f := newFixture(t)
	first := f.bundle(t)
	remote := memory()
	if _, err := push(t.Context(), f.c, remote); err != nil {
		t.Fatal(err)
	}
	latest := f.bundle(t)
	remote.uncertainReceipt = true
	_, err := push(t.Context(), f.c, remote)
	wantCode(t, err, "offsite_transport_failed")
	before := state(t, f.c)
	if len(before.Owned) != 1 || before.Owned[0].ID != first.ID || before.Pending == nil || before.Pending.ID != latest.ID || before.LastSuccess == nil {
		t.Fatal("missing genuine owned/pending recovery fixture")
	}
	lower := f.c
	lower.MaxBundleBytes = 1024
	if before.LastSuccess.ArchiveBytes <= lower.MaxBundleBytes || before.Pending.ArchiveBytes <= lower.MaxBundleBytes {
		t.Fatal("fixture archives must exceed the reduced transfer cap")
	}
	ledgerPath := filepath.Join(f.c.StateDir, "offsite.json")
	ledger, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	spoolPath := filepath.Join(f.c.StateDir, "pending.tar.gz")
	spool, err := os.ReadFile(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "offsite.json")
	if err = writeJSON(config, lower); err != nil {
		t.Fatal(err)
	}
	report, err := ReadStatus(t.Context(), config)
	if err != nil || report.LastSuccess == nil || report.Pending == nil || *report.LastSuccess != *before.LastSuccess || *report.Pending != *before.Pending || report.Retained != 1 {
		t.Fatal("reduced transfer policy invalidated historical receipts", report, err)
	}
	retained, err := readState(lower)
	if err != nil || !reflect.DeepEqual(before, retained) {
		t.Fatal("reduced cap changed ownership", retained, err)
	}
	unchanged, err := os.ReadFile(ledgerPath)
	if err != nil || !bytes.Equal(ledger, unchanged) {
		t.Fatal("status rewrote historical ledger", err)
	}
	// Retention can still recognize its owned keeper. A cap below its archive
	// size must not authorize a transfer or unchecked deletion of that copy.
	s, err := openSession(lower, remote)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.prune(t.Context(), false); err != nil || remote.deletes != 0 {
		t.Fatal("no-excess retention lost the owned keeper", err)
	}
	destination := filepath.Join(t.TempDir(), "fetch")
	_, err = s.fetch(t.Context(), first.ID, destination)
	wantCode(t, err, "offsite_archive_invalid")
	if _, err = os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("over-cap fetch left a recovery destination", err)
	}
	s.close()
	archive := filepath.Join(t.TempDir(), "too-small.tar.gz")
	_, err = pack(t.Context(), latest.Path, archive, lower)
	wantCode(t, err, "offsite_size_limit")
	if _, err = os.Lstat(archive); !os.IsNotExist(err) {
		t.Fatal("over-cap pack left an archive", err)
	}
	puts := make(map[string]int, len(remote.puts))
	for name, count := range remote.puts {
		puts[name] = count
	}
	_, err = push(t.Context(), lower, remote)
	wantCode(t, err, "offsite_pending_invalid")
	after := state(t, lower)
	if !reflect.DeepEqual(before.Owned, after.Owned) || !reflect.DeepEqual(before.Pending, after.Pending) || !reflect.DeepEqual(before.LastSuccess, after.LastSuccess) || !reflect.DeepEqual(puts, remote.puts) || remote.deletes != 0 {
		t.Fatal("over-cap continuation changed receipt ownership or remote objects")
	}
	spoolAfter, err := os.ReadFile(spoolPath)
	if err != nil || !bytes.Equal(spool, spoolAfter) {
		t.Fatal("reduced cap changed original pending archive", err)
	}
	// Restoring an adequate transfer budget resumes the exact pending identity,
	// then prunes only the original owned older copy under keep=1.
	result, err := push(t.Context(), f.c, remote)
	if err != nil || result.State != "complete" || result.BackupID != latest.ID {
		t.Fatal("could not resume retained original publication", result, err)
	}
	after = state(t, f.c)
	if after.Pending != nil || len(after.Owned) != 1 || after.Owned[0] != *before.Pending || after.LastSuccess == nil || *after.LastSuccess != *before.Pending || remote.deletes != 2 {
		t.Fatal("retention lost original ownership after cap change", after, remote.deletes)
	}
	for name, count := range puts {
		if !strings.HasSuffix(name, "/latest.json") && remote.puts[name] != count {
			t.Fatal("recovery duplicated a previously uploaded object", name)
		}
	}
}

func TestOffsiteReceiptSizeHasFixedSupportedBound(t *testing.T) {
	c := Config{InstanceID: "instance_00000000000000000000000000000001", MaxBundleBytes: 1024}
	r := Receipt{Version: 1, ID: "backup_00000000000000000000000000000001", InstanceID: c.InstanceID, SnapshotFinishedAt: "2026-01-01T00:00:00Z", ArchiveSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	for _, size := range []int64{-1, 0, 1, 1025, 1 << 40, 1<<40 + 1} {
		r.ArchiveBytes = size
		if got, want := validReceipt(r, c), size > 0 && size <= 1<<40; got != want {
			t.Fatalf("receipt size %d valid=%v, want=%v", size, got, want)
		}
	}
}
