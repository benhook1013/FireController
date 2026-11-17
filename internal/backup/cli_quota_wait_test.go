package backup

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestCLIQuotaWaitCanonicalExportInspectInactiveRestore(t *testing.T) {
	f := newRestoreFixture(t)
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"cli", "hosted"}, Fingerprint: strings.Repeat("f", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	scope := routePolicyInventoryScope(t, f)
	one := int64(1)
	if _, err := e.Configure(t.Context(), scope, "cli-wait-backup-config", reviewexec.Configuration{ExpectedRevision: &one, CapabilityID: "restore-cap", Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := e.Enqueue(t.Context(), scope, "cli-wait-backup-request", reviewexec.Request{ExpectedConfigurationRevision: 2, PullNumber: 4, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	var request reviewexec.Receipt
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	a, err := e.StartAttempt(t.Context(), f.project, request.ID)
	if err != nil || a.ID == "" || a.State != "preparing" {
		t.Fatal("start explicit CLI request", a, err)
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	anchor := reviewexec.Anchor{RepositoryID: 991, Repository: "synthetic/restore", PullNumber: 4, BaseRef: "main", CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), ParentSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("b", 40), PatchSHA256: strings.Repeat("c", 64), ObservedAt: at}
	input := reviewexec.CLIAllowanceObservation{Anchor: anchor, ObservedAt: at, Remaining: 0, Limit: 3, FullCapacityInMS: 86400000, RollingWindowMS: 86400000, ChangedFiles: 1}
	if err := e.RecordCLIExhaustion(t.Context(), f.project, a.ID, a.Revision, input); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ScheduleCLICooldown(t.Context(), f.project, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	original, err := e.ReadOperation(t.Context(), scope, request.ID)
	if err != nil || original.State != "queued" || original.AttemptID != "" || original.Wait == nil || original.Wait.BaselineAttemptID != a.ID || original.Wait.Paused {
		t.Fatal("durable quota wait", original, err)
	}
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("independent exported bundle inspection", err)
	}
	destination := filepath.Join(t.TempDir(), "inactive-cli-wait")
	restored, err := Restore(t.Context(), bundle.Path, destination)
	if err != nil || restored.State != "inactive" || restored.InstanceID == f.store.InstanceID() {
		t.Fatal("inactive restore", restored, err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	after := backupRows(t, db)
	for _, table := range []string{"review_operations", "review_operation_waits", "review_execution_attempts", "review_execution_transitions", "review_cli_preflight", "review_execution_reservations", "review_execution_evidence", "review_round_controls", "review_round_changes", "review_round_operation_bindings", "review_round_completions"} {
		if before[table] != after[table] {
			t.Fatal("restore altered queued authorization, original quota proof or review accounting", table)
		}
	}
	var state, currentAttempt, head, baseline, deadline string
	var paused bool
	if err := db.QueryRow(`SELECT o.state,o.attempt_id,o.head_sha,w.baseline_attempt_id,w.retry_not_before,w.paused FROM review_operations o JOIN review_operation_waits w ON w.operation_id=o.id WHERE o.id=?`, request.ID).Scan(&state, &currentAttempt, &head, &baseline, &deadline, &paused); err != nil || state != "queued" || currentAttempt != "" || head != anchor.CandidateSHA || baseline != a.ID || deadline != original.Wait.RetryNotBefore || paused {
		t.Fatal("restored request lost original identity/pin/timer", state, currentAttempt, baseline, deadline, err)
	}
	var remaining, evidence, completed, used, dry int64
	if err := db.QueryRow(`SELECT json_extract(body_json,'$.observation.remaining') FROM review_cli_preflight WHERE attempt_id=?`, a.ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("restored negative allowance changed", remaining, err)
	}
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?),(SELECT count(*) FROM review_round_completions WHERE operation_id=?),used,dry FROM review_round_controls WHERE project_id=? AND pull_number=4 AND channel='cli'`, a.ID, request.ID, f.project).Scan(&evidence, &completed, &used, &dry); err != nil || evidence != 0 || completed != 0 || used != 0 || dry != 0 {
		t.Fatal("quota wait gained review/taper credit", evidence, completed, used, dry, err)
	}
	recovery, err := readRecoveryStatus(t.Context(), db)
	if err != nil || recovery.State != "inactive" || !recovery.ProvidersHeld {
		t.Fatal("restore released provider work", recovery, err)
	}
	if err := reviewexec.ValidateAllowanceSnapshot(t.Context(), db, 10); err != nil {
		t.Fatal("restored historical CLI preflight validation", err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := reviewexec.ValidateOperationWaitsSnapshot(t.Context(), tx); err != nil {
		tx.Rollback()
		t.Fatal("restored queued wait validation", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	invalid := restoreModifiedBundle(t, bundle, `UPDATE review_operation_waits SET retry_not_before='2030-01-01T00:00:00Z' WHERE operation_id='`+request.ID+`'`)
	if _, err := Inspect(t.Context(), invalid); err == nil {
		t.Fatal("rehashed bundle accepted a fabricated CLI quota deadline")
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("export/inspection/restore altered the source or original bundle")
	}
}
