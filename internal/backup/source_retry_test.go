package backup

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestSourceRetryExportInspectInactiveRestoreAndCorruption(t *testing.T) {
	f := newRestoreFixture(t)
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"cli", "hosted"}, Fingerprint: strings.Repeat("f", 64)}}, reviewexec.WithSourceReadGroups(map[string]string{"restore-cap": strings.Repeat("c", 64)}))
	if err != nil {
		t.Fatal(err)
	}
	scope := routePolicyInventoryScope(t, f)
	one := int64(1)
	if _, err := e.Configure(t.Context(), scope, "source-wait-config", reviewexec.Configuration{ExpectedRevision: &one, CapabilityID: "restore-cap", Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := e.Enqueue(t.Context(), scope, "source-wait-request", reviewexec.Request{ExpectedConfigurationRevision: 2, PullNumber: 4, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	var request reviewexec.Receipt
	if err != nil || json.Unmarshal(raw, &request) != nil {
		t.Fatal(err)
	}
	a, err := e.StartAttempt(t.Context(), f.project, request.ID)
	if err != nil || a.ID == "" {
		t.Fatal(a, err)
	}
	observed := time.Now().UTC()
	failure := reviewexec.PreparationFailureAt("source_resolution", &reviewprovider.Error{Code: "provider_rate_limited", Status: 429, RetryTiming: reviewprovider.RetryTimingEvidence{State: "proven", ObservedAt: observed, NotBefore: observed.Add(time.Minute)}})
	if ok, err := e.ScheduleSourceRetry(t.Context(), f.project, a.ID, a.Revision, failure); err != nil || !ok {
		t.Fatal(ok, err)
	}
	original, err := e.ReadOperation(t.Context(), scope, request.ID)
	if err != nil || original.Wait == nil || original.Wait.SourceRetry == nil {
		t.Fatal(original, err)
	}
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("new helper inspect", err)
	}
	if executable := os.Getenv("FC_TEST_SOURCE_LEGACY_DAEMON"); executable != "" {
		beforeBundle := restoreTree(t, bundle.Path)
		output, err := exec.CommandContext(t.Context(), executable, "backup-inspect", "--bundle", bundle.Path, "--timeout=1m").CombinedOutput()
		if err == nil || !strings.Contains(string(output), "backup_invalid") {
			t.Fatal("actual prior backup helper did not refuse unknown domain", err, string(output))
		}
		if !reflect.DeepEqual(beforeBundle, restoreTree(t, bundle.Path)) {
			t.Fatal("prior helper mutated bundle")
		}
	}
	destination := filepath.Join(t.TempDir(), "inactive-source-retry")
	restored, err := Restore(t.Context(), bundle.Path, destination)
	if err != nil || restored.State != "inactive" {
		t.Fatal(restored, err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	after := backupRows(t, db)
	for _, table := range []string{"review_source_retries", "review_operations", "review_execution_attempts", "review_execution_transitions", "review_execution_reservations", "review_round_controls", "review_round_completions"} {
		if before[table] != after[table] {
			t.Fatal("restore changed retained identity/clock/accounting", table)
		}
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = reviewexec.ValidateSourceRetriesSnapshot(t.Context(), tx)
	tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`UPDATE review_source_retries SET observed_at='2000-01-01T00:00:00Z'`,
		`UPDATE review_source_retries SET retry_not_before='2099-01-01T00:00:00Z'`,
		`UPDATE review_source_retries SET source_group='private token'`,
		`UPDATE review_source_retries SET sequence=2`,
		`DELETE FROM review_source_retries`,
		`UPDATE review_execution_transitions SET reason='allowance_exhausted' WHERE attempt_id='` + a.ID + `' AND revision=2`,
		`UPDATE review_operations SET attempt_id='` + a.ID + `' WHERE id='` + request.ID + `'`,
		`DELETE FROM metadata WHERE key='schema:review_source_retries'`,
	} {
		corrupted := restoreModifiedBundle(t, bundle, sql)
		if _, err := Inspect(t.Context(), corrupted); err == nil {
			t.Fatal("accepted corrupt source retry", sql)
		}
	}
	// The prior helper's supported-domain catalog must refuse the new domain,
	// rather than claim a restore it cannot validate. Roll out matching helpers.
	version := supportedDomains["review_source_retries"]
	delete(supportedDomains, "review_source_retries")
	_, oldErr := Inspect(t.Context(), bundle.Path)
	supportedDomains["review_source_retries"] = version
	if oldErr == nil {
		t.Fatal("old helper catalog accepted unknown domain")
	}
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("matching helper failed after compatibility proof", err)
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("backup proof mutated source")
	}
}

func TestSourceRetryNewHelperAcceptsPriorSnapshotWithoutMigrating(t *testing.T) {
	f := newRestoreFixture(t)
	if _, err := f.store.DB().Exec(`DROP TABLE review_source_retries; DELETE FROM metadata WHERE key='schema:review_source_retries'`); err != nil {
		t.Fatal(err)
	}
	bundle := restoreBundle(t, f)
	before := restoreTree(t, bundle.Path)
	if executable := os.Getenv("FC_TEST_SOURCE_LEGACY_DAEMON"); executable != "" {
		output, err := exec.CommandContext(t.Context(), executable, "backup-inspect", "--bundle", bundle.Path, "--timeout=1m").CombinedOutput()
		if err != nil || !strings.Contains(string(output), "verified") {
			t.Fatal("actual prior backup helper could not inspect prior domain fixture", err, string(output))
		}
	}
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) {
		t.Fatal("inspection migrated prior snapshot")
	}
}
