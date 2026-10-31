package offsite

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestOffsiteBinaryPathUpgradeRetainsOwnershipAndPending(t *testing.T) {
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
	if len(before.Owned) != 1 || before.Owned[0].ID != first.ID || before.LastSuccess == nil || before.Pending == nil || before.Pending.ID != latest.ID {
		t.Fatal("fixture lost the original owned and pending receipts", before)
	}
	ledgerPath := filepath.Join(f.c.StateDir, "offsite.json")
	ledger, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	upgraded := f.c
	upgraded.RcloneBinary = filepath.Join(t.TempDir(), "upgraded-rclone")
	configPath := filepath.Join(t.TempDir(), "offsite-config.json")
	if err := writeJSON(configPath, upgraded); err != nil {
		t.Fatal(err)
	}
	report, err := ReadStatus(t.Context(), configPath)
	if err != nil || report.LastSuccess == nil || report.Pending == nil || *report.LastSuccess != *before.LastSuccess || *report.Pending != *before.Pending || report.Retained != len(before.Owned) {
		t.Fatal("executable upgrade invalidated the existing ownership ledger", report, err)
	}
	after, err := readState(upgraded)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("upgrade changed retained receipts or transfer state", after, err)
	}
	// The executable is replaceable; these identities still own the ledger.
	for _, test := range []struct {
		name   string
		change func(*Config)
	}{
		{"instance", func(c *Config) { c.InstanceID = "instance_" + strings.Repeat("1", 32) }},
		{"source", func(c *Config) { c.SourceRoot = filepath.Join(c.SourceRoot, "different-source") }},
		{"remote", func(c *Config) { c.Remote = "other:firecontroller" }},
		{"rclone config", func(c *Config) { c.RcloneConfig = filepath.Join(filepath.Dir(c.RcloneConfig), "different.conf") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := upgraded
			test.change(&changed)
			session, err := openSession(changed, remote)
			if session != nil {
				session.close()
				t.Fatal("changed connection opened the original ledger")
			}
			wantCode(t, err, "offsite_state_invalid")
		})
	}
	unchanged, err := os.ReadFile(ledgerPath)
	if err != nil || !bytes.Equal(ledger, unchanged) {
		t.Fatal("status or rejected rebinding rewrote the ledger", err)
	}
	// Resume the original uncertain publication through the in-memory transport.
	// Its already committed archive/receipt must not be uploaded again, and the
	// earlier owned copy must remain eligible for this ledger's keep=1 policy.
	continued, err := push(t.Context(), upgraded, remote)
	if err != nil || continued.State != "complete" || continued.BackupID != latest.ID {
		t.Fatal("upgraded executable could not continue original publication", continued, err)
	}
	after = state(t, upgraded)
	if after.Pending != nil || after.LastSuccess == nil || *after.LastSuccess != *before.Pending || len(after.Owned) != 1 || after.Owned[0] != *before.Pending || remote.deletes != 2 {
		t.Fatal("upgrade lost receipt identity or retention ownership", after, remote.deletes)
	}
	for name, count := range remote.puts {
		if strings.Contains(name, latest.ID) && count != 1 {
			t.Fatal("upgrade duplicated the original publication", name, count)
		}
	}
}
