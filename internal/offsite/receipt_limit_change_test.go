package offsite

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestOffsiteLowerTransferLimitPreservesRetainedReceipts(t *testing.T) {
	f := newFixture(t)
	first := f.bundle(t)
	remote := &receiptLimitTransport{memoryTransport: memory()}
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
	gets := remote.gets
	_, err = s.fetch(t.Context(), first.ID, destination)
	wantCode(t, err, "offsite_size_limit")
	if remote.gets != gets {
		t.Fatal("downloaded a receipt already known to exceed the cap")
	}
	if _, err = os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("over-cap fetch left a recovery destination", err)
	}
	s.close()
	// Direct archive inspection preserves cancellation priority over policy,
	// leaves its input untouched, and creates no extraction destination.
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		if cancelled {
			cancel()
		}
		output := filepath.Join(t.TempDir(), "unpacked")
		_, err = unpack(ctx, spoolPath, output, *before.Pending, lower)
		cancel()
		if cancelled {
			wantCode(t, err, "offsite_cancelled")
		} else {
			wantCode(t, err, "offsite_size_limit")
		}
		if _, err = os.Lstat(output); !os.IsNotExist(err) {
			t.Fatal("refused unpack created output", err)
		}
	}
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
	wantCode(t, err, "offsite_size_limit")
	after := state(t, lower)
	if !reflect.DeepEqual(before.Owned, after.Owned) || !reflect.DeepEqual(before.Pending, after.Pending) || !reflect.DeepEqual(before.LastSuccess, after.LastSuccess) || after.LastAttempt == nil || after.LastAttempt.Reason != "offsite_size_limit" || !reflect.DeepEqual(puts, remote.puts) || remote.deletes != 0 || remote.gets != gets {
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

// Count only actual archive downloads; receipt reads remain distinct.
type receiptLimitTransport struct {
	*memoryTransport
	gets int
}

func (m *receiptLimitTransport) get(ctx context.Context, name, destination string) error {
	m.gets++
	return m.memoryTransport.get(ctx, name, destination)
}

func TestOffsiteLowerTransferLimitPreservesRecoverySpoolPhases(t *testing.T) {
	for _, phase := range []string{"prepared", "committed-spool"} {
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t)
			bundle := f.bundle(t)
			remote := &receiptLimitTransport{memoryTransport: memory()}
			remote.uncertainReceipt = phase == "prepared"
			_, err := push(t.Context(), f.c, remote)
			if phase == "prepared" {
				wantCode(t, err, "offsite_transport_failed")
			} else if err != nil {
				t.Fatal(err)
			}
			before := state(t, f.c)
			spool := filepath.Join(f.c.StateDir, "pending.tar.gz")
			if phase == "prepared" {
				spool = filepath.Join(f.c.StateDir, "prepare.tar.gz")
				if err = os.Rename(filepath.Join(f.c.StateDir, "pending.tar.gz"), spool); err != nil {
					t.Fatal(err)
				}
			} else {
				// Recreate the exact completed spool from its genuine remote bytes,
				// as retained by a crash between success persistence and cleanup.
				name := f.c.Remote + "/" + f.c.InstanceID + "/" + bundle.ID + "/archive.tar.gz"
				if err = os.WriteFile(spool, remote.files[name], 0600); err != nil {
					t.Fatal(err)
				}
			}
			original, err := os.ReadFile(spool)
			if err != nil {
				t.Fatal(err)
			}
			gets := remote.gets
			puts := make(map[string]int, len(remote.puts))
			for name, count := range remote.puts {
				puts[name] = count
			}
			lower := f.c
			lower.MaxBundleBytes = 1024
			if int64(len(original)) <= lower.MaxBundleBytes {
				t.Fatal("fixture archive must exceed lowered cap")
			}
			_, err = push(t.Context(), lower, remote)
			wantCode(t, err, "offsite_size_limit")
			after := state(t, lower)
			if !reflect.DeepEqual(before.Pending, after.Pending) || !reflect.DeepEqual(before.LastSuccess, after.LastSuccess) || !reflect.DeepEqual(before.Owned, after.Owned) || remote.gets != gets || !reflect.DeepEqual(puts, remote.puts) || remote.deletes != 0 {
				t.Fatal("size refusal changed retained work or remote objects")
			}
			retained, err := os.ReadFile(spool)
			if err != nil || !bytes.Equal(retained, original) {
				t.Fatal("size refusal changed spool", err)
			}
			if phase == "prepared" {
				if _, err = os.Lstat(filepath.Join(f.c.StateDir, "pending.tar.gz")); !os.IsNotExist(err) {
					t.Fatal("size refusal renamed preparation", err)
				}
			}
			result, err := push(t.Context(), f.c, remote)
			if err != nil || result.BackupID != bundle.ID || state(t, f.c).Pending != nil {
				t.Fatal("adequate cap did not recover original identity", result, err)
			}
			if _, err = os.Lstat(spool); !os.IsNotExist(err) {
				t.Fatal("successful recovery retained obsolete spool", err)
			}
		})
	}
}

func TestOffsiteLowerTransferLimitRetentionPreservesOwnedCopies(t *testing.T) {
	f := newFixture(t)
	f.c.Keep = 2
	remote := &receiptLimitTransport{memoryTransport: memory()}
	for range 2 {
		f.bundle(t)
		if _, err := push(t.Context(), f.c, remote); err != nil {
			t.Fatal(err)
		}
	}
	before := state(t, f.c)
	lower := f.c
	lower.Keep = 1
	lower.MaxBundleBytes = 1024
	for _, alreadyVerified := range []bool{false, true} {
		s, err := openSession(lower, remote)
		if err != nil {
			t.Fatal(err)
		}
		gets := remote.gets
		err = s.prune(t.Context(), alreadyVerified)
		wantCode(t, err, "offsite_size_limit")
		s.close()
		if remote.gets != gets || remote.deletes != 0 || !reflect.DeepEqual(state(t, f.c), before) {
			t.Fatal("over-cap retention downloaded, deleted, or rewrote history")
		}
	}
}
