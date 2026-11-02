package offsite

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/backuphealth"
)

func TestOffsiteFetchDistinguishesExistingDestinationFromStorageFailure(t *testing.T) {
	f := newFixture(t)
	f.bundle(t)
	m := memory()
	if _, err := push(t.Context(), f.c, m); err != nil {
		t.Fatal(err)
	}
	s, err := openSession(f.c, m)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	existing := filepath.Join(t.TempDir(), "existing")
	if err := os.WriteFile(existing, []byte("KEEP"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = s.fetch(t.Context(), "", existing)
	wantCode(t, err, "offsite_destination_exists")
	data, err := os.ReadFile(existing)
	if err != nil || string(data) != "KEEP" {
		t.Fatal("existing destination changed", err)
	}
	for _, destination := range []string{filepath.Join(t.TempDir(), "missing-parent", "destination"), filepath.Join(existing, "destination")} {
		_, err = s.fetch(t.Context(), "", destination)
		wantCode(t, err, "offsite_storage_unavailable")
	}
}

func TestOffsiteDeleteRefusalAllowsFreshCopiesUntilInventoryBound(t *testing.T) {
	f := newFixture(t)
	first := f.bundle(t)
	m := memory()
	if _, err := push(t.Context(), f.c, m); err != nil {
		t.Fatal(err)
	}
	second := f.bundle(t)
	m.failDelete = true
	_, err := push(t.Context(), f.c, m)
	wantCode(t, err, "offsite_transport_failed")
	unchanged, err := push(t.Context(), f.c, m)
	wantCode(t, err, "offsite_transport_failed")
	if unchanged.State != "unchanged" || unchanged.BackupID != second.ID || len(state(t, f.c).Owned) != 2 {
		t.Fatal("unchanged source hid cleanup failure or duplicated owned backup", unchanged, state(t, f.c))
	}
	third := f.bundle(t)
	_, err = push(t.Context(), f.c, m)
	wantCode(t, err, "offsite_transport_failed")
	st := state(t, f.c)
	if st.LastSuccess.ID != third.ID || len(st.Owned) != 3 || st.RetentionReason != "offsite_retention_failed" || m.deletes != 0 {
		t.Fatal("old delete refusal blocked fresh verified publication", st, m.deletes)
	}
	for _, id := range []string{first.ID, second.ID, third.ID} {
		for name, n := range m.puts {
			if strings.Contains(name, id) && n != 1 {
				t.Fatal("committed backup duplicated", name, n)
			}
		}
	}
	m.failDelete = false
	fourth := f.bundle(t)
	result, err := push(t.Context(), f.c, m)
	if err != nil || result.State != "complete" || result.BackupID != fourth.ID || state(t, f.c).RetentionReason != "" || len(state(t, f.c).Owned) != 1 {
		t.Fatal("transient retention did not converge", result, err, state(t, f.c))
	}

	// Trusted retained receipts exercise the real128 bound without128 DB
	// snapshots. The newest keeper remains an actual verified bundle.
	st = state(t, f.c)
	keeper := *st.LastSuccess
	archive := m.files[f.c.Remote+"/"+f.c.InstanceID+"/"+keeper.ID+"/archive.tar.gz"]
	st.Owned = []Receipt{keeper}
	for index := 0; index < maxOwned-2; index++ {
		r := keeper
		r.ID = fmt.Sprintf("backup_%032x", index+1000)
		r.EventSequence = 0
		st.Owned = append(st.Owned, r)
		prefix := f.c.Remote + "/" + f.c.InstanceID + "/" + r.ID + "/"
		body, _ := json.Marshal(r)
		m.files[prefix+"receipt.json"] = body
		m.files[prefix+"archive.tar.gz"] = archive
	}
	if err := writeJSON(filepath.Join(f.c.StateDir, "offsite.json"), st); err != nil {
		t.Fatal(err)
	}
	m.failDelete = true
	lastAllowed := f.bundle(t)
	_, err = push(t.Context(), f.c, m)
	wantCode(t, err, "offsite_transport_failed")
	if got := state(t, f.c); len(got.Owned) != maxOwned || got.LastSuccess.ID != lastAllowed.ID {
		t.Fatal("bound was not reachable", got)
	}
	blocked := f.bundle(t)
	_, err = push(t.Context(), f.c, m)
	wantCode(t, err, "offsite_retention_limit")
	if got := state(t, f.c); len(got.Owned) != maxOwned || got.LastSuccess.ID != lastAllowed.ID || got.Pending != nil || got.RetentionReason != "offsite_retention_failed" {
		t.Fatal("inventory cap changed ownership/publication", got)
	}
	for name := range m.puts {
		if strings.Contains(name, blocked.ID) {
			t.Fatal("uploaded beyond retained inventory cap", name)
		}
	}
	// The supported ledger decoder also accepts a retained pending intent at
	// capacity. Refuse before publication while retaining its exact recovery data.
	spool := filepath.Join(f.c.StateDir, "pending.tar.gz")
	pending, err := pack(t.Context(), blocked.Path, spool, f.c)
	if err != nil {
		t.Fatal(err)
	}
	full := state(t, f.c)
	full.Pending = &pending
	if err := writeJSON(filepath.Join(f.c.StateDir, "offsite.json"), full); err != nil {
		t.Fatal(err)
	}
	accepted := state(t, f.c)
	beforeSpool, err := os.ReadFile(spool)
	if err != nil {
		t.Fatal(err)
	}
	beforePuts := map[string]int{}
	for key, value := range m.puts {
		beforePuts[key] = value
	}
	beforeDeletes := m.deletes
	_, err = push(t.Context(), f.c, m)
	wantCode(t, err, "offsite_retention_limit")
	after := state(t, f.c)
	afterSpool, err := os.ReadFile(spool)
	if err != nil || len(after.Owned) != maxOwned || !reflect.DeepEqual(after.Owned, accepted.Owned) || !reflect.DeepEqual(after.Pending, accepted.Pending) || !reflect.DeepEqual(after.LastSuccess, accepted.LastSuccess) || !bytes.Equal(beforeSpool, afterSpool) || !reflect.DeepEqual(m.puts, beforePuts) || m.deletes != beforeDeletes {
		t.Fatal("at-cap pending refusal lost recovery state or published129th object", after, err)
	}
}

type retentionBoundaryTransport struct {
	*memoryTransport
	removeHook func(string) error
	readHook   func()
}

func (m *retentionBoundaryTransport) remove(ctx context.Context, name string) error {
	if m.removeHook != nil {
		if err := m.removeHook(name); err != nil {
			return err
		}
	}
	return m.memoryTransport.remove(ctx, name)
}
func (m *retentionBoundaryTransport) read(ctx context.Context, name string) ([]byte, error) {
	body, err := m.memoryTransport.read(ctx, name)
	if m.readHook != nil {
		m.readHook()
	}
	return body, err
}

func TestOffsitePartialDeletionRetainsOwnershipUntilExactCleanup(t *testing.T) {
	f := newFixture(t)
	first := f.bundle(t)
	m := &retentionBoundaryTransport{memoryTransport: memory()}
	if _, err := push(t.Context(), f.c, m); err != nil {
		t.Fatal(err)
	}
	m.removeHook = func(name string) error {
		if strings.HasSuffix(name, "receipt.json") {
			return failure("offsite_transport_failed")
		}
		return nil
	}
	latest := f.bundle(t)
	_, err := push(t.Context(), f.c, m)
	wantCode(t, err, "offsite_transport_failed")
	st := state(t, f.c)
	prefix := f.c.Remote + "/" + f.c.InstanceID + "/" + first.ID + "/"
	if len(st.Owned) != 2 || st.LastSuccess.ID != latest.ID || m.files[prefix+"archive.tar.gz"] != nil || m.files[prefix+"receipt.json"] == nil {
		t.Fatal("partial delete lost ownership proof", st)
	}
	m.removeHook = nil
	result, err := push(t.Context(), f.c, m)
	if err != nil || result.State != "unchanged" || len(state(t, f.c).Owned) != 1 || m.files[prefix+"receipt.json"] != nil || state(t, f.c).RetentionReason != "" {
		t.Fatal("partial deletion could not resume exact cleanup", result, err, state(t, f.c))
	}
}

func TestOffsiteRetentionIntegrityCancellationAndSaveFailuresRemainTerminal(t *testing.T) {
	for _, kind := range []string{"keeper", "old-object", "cancellation", "save"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			old := f.bundle(t)
			remote := memory()
			if _, err := push(t.Context(), f.c, remote); err != nil {
				t.Fatal(err)
			}
			keeper := f.bundle(t)
			remote.failDelete = true
			_, err := push(t.Context(), f.c, remote)
			wantCode(t, err, "offsite_transport_failed")
			before := state(t, f.c)
			fresh := f.bundle(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			m := &retentionBoundaryTransport{memoryTransport: remote}
			code := "offsite_remote_conflict"
			switch kind {
			case "keeper":
				remote.corruptDownload = true
				code = "offsite_archive_invalid"
				before.RetentionReason = ""
				if err := writeJSON(filepath.Join(f.c.StateDir, "offsite.json"), before); err != nil {
					t.Fatal(err)
				}
			case "old-object":
				remote.files[f.c.Remote+"/"+f.c.InstanceID+"/"+old.ID+"/archive.tar.gz"] = []byte("CHANGED")
			case "cancellation":
				m.readHook = cancel
				code = "offsite_cancelled"
			case "save":
				code = "offsite_storage_unavailable"
				ledger := filepath.Join(f.c.StateDir, "offsite.json")
				saved := ledger + ".test-original"
				m.removeHook = func(string) error {
					if err := os.Rename(ledger, saved); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(ledger, 0700); err != nil {
						t.Fatal(err)
					}
					return failure("offsite_transport_failed")
				}
				defer func() {
					if err := os.Remove(ledger); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(saved, ledger); err != nil {
						t.Fatal(err)
					}
				}()
			}
			_, err = push(ctx, f.c, m)
			wantCode(t, err, code)
			if remote.deletes != 0 {
				t.Fatal("unverified/canceled/local-failure retention deleted objects")
			}
			for name := range remote.puts {
				if strings.Contains(name, fresh.ID) {
					t.Fatal("unsafe retention failure allowed new upload", kind, name)
				}
			}
			if kind != "save" {
				after := state(t, f.c)
				if after.LastSuccess.ID != keeper.ID || !reflect.DeepEqual(before.Owned, after.Owned) || after.RetentionReason != "offsite_retention_failed" {
					t.Fatal("failure lost ownership/warning", before, after)
				}
			}
			if kind == "old-object" && !bytes.Equal(remote.files[f.c.Remote+"/"+f.c.InstanceID+"/"+old.ID+"/archive.tar.gz"], []byte("CHANGED")) {
				t.Fatal("changed old object was overwritten")
			}
		})
	}
}

func TestOffsiteUnchangedSourceClearsObsoleteRetentionWarningAfterKeepIncrease(t *testing.T) {
	f := newFixture(t)
	f.bundle(t)
	deleteCalls := 0
	remote := &retentionBoundaryTransport{memoryTransport: memory(), removeHook: func(string) error { deleteCalls++; return nil }}
	if _, err := push(t.Context(), f.c, remote); err != nil {
		t.Fatal(err)
	}
	latest := f.bundle(t)
	remote.failDelete = true
	_, err := push(t.Context(), f.c, remote)
	wantCode(t, err, "offsite_transport_failed")
	if deleteCalls == 0 {
		t.Fatal("fixture did not attempt an actual owned-object deletion")
	}
	// The same source must retain its warning while cleanup is still required.
	unchanged, err := push(t.Context(), f.c, remote)
	wantCode(t, err, "offsite_transport_failed")
	before := state(t, f.c)
	if unchanged.State != "unchanged" || len(before.Owned) != 2 || before.RetentionReason != "offsite_retention_failed" || before.Pending != nil || before.LastSuccess == nil || before.LastSuccess.ID != latest.ID {
		t.Fatal("pending cleanup warning was lost", unchanged, before)
	}
	config := filepath.Join(t.TempDir(), "offsite.json")
	if err := writeJSON(config, f.c); err != nil {
		t.Fatal(err)
	}
	reportDir := t.TempDir()
	if err := os.Chmod(reportDir, 0700); err != nil {
		t.Fatal(err)
	}
	healthPath := filepath.Join(reportDir, "health.json")
	failed, err := ReadStatusAndWriteHealth(t.Context(), config, healthPath, "Cloud backup", time.Hour)
	if err != nil || failed.RetentionReason != "offsite_retention_failed" {
		t.Fatal("retention failure missing from report", failed, err)
	}
	mirror, err := backuphealth.ReadMirror(healthPath)
	if err != nil || !mirror.RetentionWarning || backuphealth.Offsite(mirror, f.c.InstanceID, time.Now()).State != "failed" {
		t.Fatal("retention failure missing from health", mirror, err)
	}
	beforePuts := make(map[string]int)
	for name, count := range remote.puts {
		beforePuts[name] = count
	}
	beforeFiles := make(map[string][]byte)
	for name, body := range remote.files {
		beforeFiles[name] = bytes.Clone(body)
	}
	beforeDeletes, beforeDeleteCalls := remote.deletes, deleteCalls
	f.c.Keep = len(before.Owned)
	if err := writeJSON(config, f.c); err != nil {
		t.Fatal(err)
	}
	result, err := push(t.Context(), f.c, remote)
	if err != nil || result.State != "unchanged" || result.BackupID != latest.ID || result.Retained != 2 {
		t.Fatal("changed Keep did not recover unchanged push", result, err)
	}
	after := state(t, f.c)
	if after.RetentionReason != "" || after.LastAttempt.State != "unchanged" || after.LastAttempt.Reason != "" || !reflect.DeepEqual(after.LastSuccess, before.LastSuccess) || !reflect.DeepEqual(after.Owned, before.Owned) || !reflect.DeepEqual(after.Pending, before.Pending) {
		t.Fatal("obsolete warning remained or verified ownership changed", before, after)
	}
	if !reflect.DeepEqual(remote.puts, beforePuts) || !reflect.DeepEqual(remote.files, beforeFiles) || remote.deletes != beforeDeletes || deleteCalls != beforeDeleteCalls {
		t.Fatal("Keep increase rewrote or deleted remote objects")
	}
	report, err := ReadStatusAndWriteHealth(t.Context(), config, healthPath, "Cloud backup", time.Hour)
	if err != nil || report.RetentionReason != "" || !reflect.DeepEqual(report.LastSuccess, before.LastSuccess) || report.LastAttempt.State != "unchanged" {
		t.Fatal("retained status did not recover", report, err)
	}
	mirror, err = backuphealth.ReadMirror(healthPath)
	if err != nil || mirror.RetentionWarning || mirror.AttemptState != "unchanged" || mirror.LastSnapshotAt != before.LastSuccess.SnapshotFinishedAt || backuphealth.Offsite(mirror, f.c.InstanceID, time.Now()).State != "healthy" {
		t.Fatal("health did not recover without inventing a fresh snapshot", mirror, err)
	}
}
