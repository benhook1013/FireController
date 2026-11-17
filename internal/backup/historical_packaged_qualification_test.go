//go:build offsite_integration

package backup

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Explicit opt-in qualification, never a live restore. Linux namespaces remove
// all network interfaces except a down loopback device; PATH is empty, and HOME
// contains no credentials. The synthetic predecessor retains its old schema.
func TestHistoricalSourcePackagedOfflineRestore(t *testing.T) {
	operator := os.Getenv("FC_TEST_FCD")
	if operator == "" {
		t.Skip("set FC_TEST_FCD to the packaged test executable")
	}
	if runtime.GOOS != "linux" {
		t.Skip("Linux namespace qualification")
	}
	if !filepath.IsAbs(operator) {
		t.Fatal("explicit absolute packaged executable required")
	}
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Fatal("offline qualification requires unshare", err)
	}
	for _, predecessor := range []bool{false} {
		name := "source-0bcc2ac1"
		if predecessor {
			name = "adoption-v1"
		}
		t.Run(name, func(t *testing.T) {
			f := newRestoreFixture(t)
			bundle := restoreBundle(t, f)
			expectedRows := backupRows(t, f.store.DB())
			const stableMetadata = "SELECT key,value FROM metadata WHERE key NOT IN ('instance_id','signing_key','restore:state','restore:source_instance_id','restore:bundle_id','restore:snapshot_started_at','restore:snapshot_finished_at','restore:event_sequence') ORDER BY key"
			const schemaSQL = "SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name"
			expectedMetadata := restoreRows(t, f.store.DB(), stableMetadata)
			expectedSQL := restoreRows(t, f.store.DB(), schemaSQL)
			var columns []string
			for _, row := range restoreRows(t, f.store.DB(), "SELECT name FROM pragma_table_info('credentials') ORDER BY cid") {
				name := row[0].(string)
				if name != "revoked" {
					columns = append(columns, name)
				}
			}
			credentialSQL := "SELECT " + strings.Join(columns, ",") + " FROM credentials ORDER BY id"
			expectedCredentials := restoreRows(t, f.store.DB(), credentialSQL)
			if len(expectedCredentials) == 0 {
				t.Fatal("empty credential fixture")
			}
			var oldKey string
			if err := f.store.DB().QueryRow("SELECT value FROM metadata WHERE key='signing_key'").Scan(&oldKey); err != nil {
				t.Fatal(err)
			}
			expectedSchemas := restoreRows(t, f.store.DB(), "SELECT key,value FROM metadata WHERE key LIKE 'schema:%' ORDER BY key")
			delete(expectedRows, "metadata")
			delete(expectedRows, "credentials")
			sealed := restoreTree(t, bundle.Path)
			var originalVersion string
			if err := f.store.DB().QueryRow("SELECT value FROM metadata WHERE key='schema:review_adoption_obligations'").Scan(&originalVersion); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "inactive")
			cmd := exec.CommandContext(t.Context(), unshare, "--user", "--map-root-user", "--net", operator, "restore", "--bundle", bundle.Path, "--data-dir", destination)
			cmd.Env = []string{"PATH=", "HOME=" + t.TempDir()}
			var diagnostic bytes.Buffer
			cmd.Stderr = &diagnostic
			output, err := cmd.Output()
			if err != nil {
				t.Fatalf("network-isolated packaged restore: %v %s", err, diagnostic.String())
			}
			var restored RestoreSummary
			if err := json.Unmarshal(output, &restored); err != nil || restored.State != "inactive" {
				t.Fatal("inactive packaged restore result", err)
			}
			db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if !reflect.DeepEqual(expectedCredentials, restoreRows(t, db, credentialSQL)) {
				t.Fatal("credential history changed beyond revocation")
			}
			if !reflect.DeepEqual(expectedMetadata, restoreRows(t, db, stableMetadata)) {
				t.Fatal("unrelated metadata changed")
			}
			if !reflect.DeepEqual(expectedSQL, restoreRows(t, db, schemaSQL)) {
				t.Fatal("physical schema definitions changed")
			}
			if restored.InstanceID == "" || restored.InstanceID == bundle.SourceInstanceID || restored.SourceInstanceID != bundle.SourceInstanceID || restored.BundleID != bundle.ID {
				t.Fatal("fresh identity or provenance invalid")
			}
			expectedRecovery := map[string]string{"instance_id": restored.InstanceID, "restore:state": "inactive", "restore:source_instance_id": bundle.SourceInstanceID, "restore:bundle_id": bundle.ID, "restore:snapshot_started_at": bundle.SnapshotStartedAt, "restore:snapshot_finished_at": bundle.SnapshotFinishedAt, "restore:event_sequence": strconv.FormatInt(bundle.EventSequence, 10)}
			for key, want := range expectedRecovery {
				var got string
				if err := db.QueryRow("SELECT value FROM metadata WHERE key=?", key).Scan(&got); err != nil || got != want {
					t.Fatal("recovery metadata mismatch", key, err)
				}
			}
			var newKey string
			if err := db.QueryRow("SELECT value FROM metadata WHERE key='signing_key'").Scan(&newKey); err != nil || newKey == "" || newKey == oldKey {
				t.Fatal("signing key not rotated", err)
			}
			actualRows := backupRows(t, db)
			delete(actualRows, "metadata")
			delete(actualRows, "credentials")
			if !reflect.DeepEqual(expectedSchemas, restoreRows(t, db, "SELECT key,value FROM metadata WHERE key LIKE 'schema:%' ORDER BY key")) {
				t.Fatal("schema vector changed")
			}
			var activeCredentials int
			if err := db.QueryRow("SELECT count(*) FROM credentials WHERE revoked != 1").Scan(&activeCredentials); err != nil || activeCredentials != 0 {
				t.Fatal("predecessor credentials not revoked", err)
			}
			if !reflect.DeepEqual(expectedRows, actualRows) {
				t.Fatal("retained historical rows changed")
			}
			var version string
			if err := db.QueryRow("SELECT value FROM metadata WHERE key='schema:review_adoption_obligations'").Scan(&version); err != nil || version != originalVersion {
				t.Fatal("restore migrated schema", version, err)
			}
			state, err := readRecoveryStatus(t.Context(), db)
			if err != nil || !state.ProvidersHeld || state.State != "inactive" {
				t.Fatal("provider recovery fence", state, err)
			}
			if !reflect.DeepEqual(sealed, restoreTree(t, bundle.Path)) {
				t.Fatal("restore modified sealed input")
			}
		})
	}
}
