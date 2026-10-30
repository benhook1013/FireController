package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func queuedCooldown(t *testing.T) (*attemptFixture, Attempt, HostedAllowanceInput, *time.Time) {
	t.Helper()
	f, a, in := cooldownObservationFixture(t)
	now := time.Now().UTC()
	f.e.schedulingNow = func() time.Time { return now }
	if err := f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.ScheduleHostedCooldown(t.Context(), attemptProject, a.ID, 1); err != nil {
		t.Fatal(err)
	}
	return f, a, in, &now
}

func TestCooldownSchemaRetainsDependentRows(t *testing.T) {
	f, a, in := cooldownObservationFixture(t)
	if err := f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal(err)
	}
	// Retain real CLI preflight and final evidence beside the preparing
	// Hosted query/observation/reservation, exercising every dependent table.
	cli := f.start(t, attemptProject, f.enqueue(t, attemptProject, "migration-cli", "cli", 4, 1))
	anchor := attemptAnchor(attemptProject, 4)
	if err := f.e.RecordCLIAllowance(t.Context(), attemptProject, cli.ID, 1, CLIAllowanceObservation{Anchor: anchor, ObservedAt: stamp(), Remaining: 3, Limit: 5, FullCapacityInMS: 12000, RollingWindowMS: 3600000, ChangedFiles: 2}); err != nil {
		t.Fatal(err)
	}
	cli, err := f.e.Admit(t.Context(), attemptProject, cli.ID, 1, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("1", 64)
	capture := ReviewEvidenceInput{CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1024, StartedAt: stamp(), FinishedAt: stamp(), ExpectedAnchor: anchor, Review: reviewprovider.ParsedReview{Outcome: "completed", Complete: true, ReportedFindings: 0, ReviewedFiles: []string{"changed.go"}}}
	if _, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, cli.ID, capture); err != nil {
		t.Fatal(err)
	}

	tables := []string{"review_execution_attempts", "review_execution_reservations", "review_execution_transitions", "review_cli_preflight", "review_hosted_preflight", "review_hosted_allowance_queries", "review_execution_evidence", "review_hosted_observations", "review_operations", "receipts", "events"}
	before := map[string]string{}
	for _, table := range tables {
		before[table] = snapshotTable(t, f, table)
	}
	restorePreCooldownAttemptSchema(t, f)
	admissionSQL(t, f, `UPDATE metadata SET value='8' WHERE key='schema:review_execution'`)
	if err := EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal("retained migration", err)
	}
	for _, table := range tables {
		if after := snapshotTable(t, f, table); after != before[table] {
			t.Fatalf("migration rewrote %s", table)
		}
	}
	if err := EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal("idempotent migration", err)
	}
	if err := validateExecutionForeignKeysTest(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}

func snapshotTable(t *testing.T, f *attemptFixture, table string) string {
	t.Helper()
	rows, err := f.store.DB().Query(`SELECT * FROM ` + table + ` ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	result := [][]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(result)
	return string(b)
}
func validateExecutionForeignKeysTest(ctx context.Context, db *sql.DB) error {
	c, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	return validateExecutionForeignKeys(ctx, c)
}

func TestCooldownWaitDueRestartAndOriginalHistory(t *testing.T) {
	f, a, in, now := queuedCooldown(t)
	scope := f.scope(t, attemptProject, f.writer, false)
	o := bindingOperation(t, f, scope, a.OperationID)
	if o.State != "queued" || o.AttemptID != "" || o.Wait == nil || o.Wait.BaselineAttemptID != a.ID || o.Wait.RetryNotBefore != in.Observation.RetryNotBefore.Format(time.RFC3339Nano) || o.Wait.Paused {
		t.Fatal("wait", o)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("cooldown retained capacity")
	}
	original := snapshotTable(t, f, "review_hosted_preflight") + snapshotTable(t, f, "review_hosted_allowance_queries") + snapshotTable(t, f, "review_hosted_observations")
	pending, err := f.e.pending(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatal("early scheduler", pending, err)
	}
	early, err := f.e.StartAttempt(t.Context(), attemptProject, o.ID)
	if err != nil || early.ID != "" {
		t.Fatal("early attempt", early, err)
	}
	cliAllowanceReopen(t, f)
	f.e.schedulingNow = func() time.Time { return *now }
	if err := f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := bindingOperation(t, f, scope, a.OperationID)
	if !reflect.DeepEqual(o, after) {
		t.Fatal("restart changed wait", after)
	}
	*now = in.Observation.RetryNotBefore
	pending, err = f.e.pending(t.Context())
	if err != nil || len(pending) != 1 {
		t.Fatal("due scheduler", pending, err)
	}
	next := f.start(t, attemptProject, Receipt{ID: o.ID})
	if next.ID == a.ID || next.OperationID != a.OperationID {
		t.Fatal("retry identity", next)
	}
	baseline, err := f.e.RetryBaseline(t.Context(), attemptProject, o.ID)
	if err != nil || baseline == nil || *baseline != in.Anchor {
		t.Fatal("baseline", baseline, err)
	}
	if records, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID); err != nil || len(records) != 1 {
		t.Fatal("old observations", records, err)
	}
	if err := f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal("historical replay", err)
	}
	if original != snapshotTable(t, f, "review_hosted_preflight")+snapshotTable(t, f, "review_hosted_allowance_queries")+snapshotTable(t, f, "review_hosted_observations") {
		t.Fatal("retry rewrote prior source")
	}

	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts WHERE operation_id=?`, o.ID) != 2 {
		t.Fatal("attempt history")
	}
}

func timerControl(t *testing.T, f *attemptFixture, id, action, key string) OperationControlReceipt {
	t.Helper()
	scope := f.scope(t, attemptProject, f.writer, false)
	o := bindingOperation(t, f, scope, id)
	raw, err := f.e.ControlOperation(t.Context(), scope, key, id, OperationControl{ExpectedRevision: o.Revision, Action: action})
	if err != nil {
		t.Fatal(action, err)
	}
	var receipt OperationControlReceipt
	if json.Unmarshal(raw, &receipt) != nil {
		t.Fatal("control receipt")
	}
	return receipt
}

func TestCooldownControlPauseResumeCancelAndProjectPause(t *testing.T) {
	for _, scenario := range []string{"operation pause", "project pause", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			f, a, in, now := queuedCooldown(t)
			scope := f.scope(t, attemptProject, f.writer, false)
			switch scenario {
			case "operation pause":
				timerControl(t, f, a.OperationID, "pause", "pause")
			case "project pause":
				f.configure(t, attemptProject, "test-capability", 1, true)
			case "cancel":
				timerControl(t, f, a.OperationID, "cancel", "cancel")
			}
			*now = in.Observation.RetryNotBefore.Add(time.Hour)
			pending, err := f.e.pending(t.Context())
			if err != nil || len(pending) != 0 {
				t.Fatal("paused/cancelled scheduled", pending, err)
			}
			got, err := f.e.StartAttempt(t.Context(), attemptProject, a.OperationID)
			if scenario == "cancel" {
				attemptCode(t, err, "review_conflict")
				o := bindingOperation(t, f, scope, a.OperationID)
				if o.State != "cancelled" || o.Wait != nil {
					t.Fatal("cancel projection", o)
				}
				return
			}
			if err != nil || got.ID != "" {
				t.Fatal("paused started", got, err)
			}
			if scenario == "project pause" {
				f.configure(t, attemptProject, "test-capability", 2, false)
			} else {
				timerControl(t, f, a.OperationID, "resume", "resume")
			}
			next := f.start(t, attemptProject, Receipt{ID: a.OperationID})
			if next.ID == a.ID {
				t.Fatal("resume reused attempt")
			}
			o := bindingOperation(t, f, scope, a.OperationID)
			if o.ConfigurationRevision != 1 || o.Wait != nil {
				t.Fatal("resume projection/provenance", o)
			}
			_, err = f.e.ControlOperation(t.Context(), scope, "too-late", o.ID, OperationControl{ExpectedRevision: o.Revision, Action: "cancel"})
			attemptCode(t, err, "review_operation_started")
		})
	}
}

func TestCooldownDueRechecksCurrentAuthority(t *testing.T) {
	for _, scenario := range []string{"revoked", "policy", "capability", "configuration", "closed allocation"} {
		t.Run(scenario, func(t *testing.T) {
			f, a, in, now := queuedCooldown(t)
			switch scenario {
			case "revoked":
				admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
			case "policy":
				admissionPolicy(t, f, 1)
			case "capability":
				bindEngine(t, f, Capability{ID: "test-capability", Provider: "coderabbit", Account: "changed-account", Channels: []string{"cli", "hosted"}})
			case "configuration":
				f.configure(t, attemptProject, "other-account", 1, false)
			case "closed allocation":
				roundGrant(t, f, 2, "hosted", 0, 0, 3, "grant")
				revision := int64(1)
				if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "close", 2, "hosted", RoundControlRequest{ExpectedRevision: &revision, Action: "close"}); err != nil {
					t.Fatal(err)
				}
			}
			f.e.schedulingNow = func() time.Time { return *now }
			*now = in.Observation.RetryNotBefore
			next, err := f.e.StartAttempt(t.Context(), attemptProject, a.OperationID)
			if err != nil || next.ID != "" {
				t.Fatal("changed authority started", next, err)
			}
			var state, reason string
			if err := f.store.DB().QueryRow(`SELECT state,reason FROM review_operations WHERE id=?`, a.OperationID).Scan(&state, &reason); err != nil {
				t.Fatal(err)
			}
			if state != "held" || reason == "" {
				t.Fatal("missing explicit hold", state, reason)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts WHERE operation_id=?`, a.OperationID) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("authority refusal allocated retry")
			}
		})
	}
}

func TestCooldownWaitProgressAndWireOptIn(t *testing.T) {
	f, a, _, _ := queuedCooldown(t)
	progress, err := f.e.ReadRoundProgressStages(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), []int64{2})
	if err != nil || progress.Items[0].Channels[0].Wait == nil || progress.Items[0].Channels[1].Wait != nil {
		t.Fatal("current channel wait", progress, err)
	}
	for _, path := range []string{"/api/v1/projects/" + attemptProject + "/reviews/operations/" + a.OperationID, "/api/v1/projects/" + attemptProject + "/reviews/operations/" + a.OperationID + "/status", roundProgressPath(attemptProject) + "?pull_numbers=2&view=stages"} {
		for _, opt := range []bool{false, true} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("Authorization", "Bearer "+f.reader)
			if opt {
				r.Header.Set(operationWaitHeader, "1")
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			if w.Code != 200 || strings.Contains(w.Body.String(), `"wait":`) != opt {
				t.Fatal("wire opt-in", path, opt, w.Code, w.Body.String())
			}
		}
	}
}

func TestCooldownSnapshotRejectsCorruptWaitOwnership(t *testing.T) {
	for _, scenario := range []string{"valid queued", "valid current retry", "deadline", "baseline", "queued pointer", "reservation", "evidence", "terminal held time"} {
		t.Run(scenario, func(t *testing.T) {
			f, a, in, now := queuedCooldown(t)
			switch scenario {
			case "valid current retry":
				*now = in.Observation.RetryNotBefore
				f.start(t, attemptProject, Receipt{ID: a.OperationID})
			case "deadline":
				admissionSQL(t, f, `UPDATE review_operation_waits SET retry_not_before=?`, in.Observation.RetryNotBefore.Add(time.Hour).Format(time.RFC3339Nano))
			case "baseline":
				admissionSQL(t, f, `UPDATE review_operations SET attempt_id=? WHERE id=?`, a.ID, a.OperationID)
			case "queued pointer":
				admissionSQL(t, f, `UPDATE review_operations SET state='active' WHERE id=?`, a.OperationID)
			case "reservation":
				admissionSQL(t, f, `INSERT INTO review_execution_reservations(provider,repository_id,pull_number,action_class,account,attempt_id) VALUES('coderabbit',801,2,'hosted','test-account',?)`, a.ID)
			case "evidence":
				admissionSQL(t, f, `INSERT INTO review_execution_evidence(id,project_id,attempt_id,operation_id,body_json,fingerprint) VALUES('corrupt',?,?,?,'{}','corrupt')`, attemptProject, a.ID, a.OperationID)
			case "terminal held time":
				admissionSQL(t, f, `UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=? AND revision=2`, in.Observation.RetryNotBefore.Format(time.RFC3339Nano), a.ID)
			}
			tx, err := f.store.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			err = ValidateOperationWaitsSnapshot(t.Context(), tx)
			if strings.HasPrefix(scenario, "valid") {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("corrupt wait accepted")
			}
		})
	}
}

// Restore the actual schema8 attempt cardinality only in a disposable fixture.
// Other migration regressions invoke this before replaying older migrations.
func restorePreCooldownAttemptSchema(t *testing.T, f *attemptFixture) {
	t.Helper()
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	statements := []string{`PRAGMA foreign_keys=OFF`, `DROP TABLE review_operation_waits`, `DROP INDEX review_attempts_operation`, `DROP INDEX review_attempts_live_operation`, `CREATE TABLE old_attempts(id TEXT PRIMARY KEY,operation_id TEXT NOT NULL UNIQUE REFERENCES review_operations(id),project_id TEXT NOT NULL REFERENCES projects(id),provider TEXT NOT NULL,account TEXT NOT NULL,state TEXT NOT NULL,revision INTEGER NOT NULL,anchor_json TEXT NOT NULL,intent TEXT NOT NULL,external_id TEXT NOT NULL)`, `INSERT INTO old_attempts SELECT * FROM review_execution_attempts`, `DROP TABLE review_execution_attempts`, `ALTER TABLE old_attempts RENAME TO review_execution_attempts`, `PRAGMA foreign_keys=ON`}
	for _, statement := range statements {
		if _, err := conn.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(statement, err)
		}
	}
}

func TestCooldownSchemaRejectsDanglingReferencesAndRollsBackFailures(t *testing.T) {
	for _, scenario := range []string{"dangling child", "version write failure"} {
		t.Run(scenario, func(t *testing.T) {
			f, a, in := cooldownObservationFixture(t)
			if err := f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, in); err != nil {
				t.Fatal(err)
			}
			restorePreCooldownAttemptSchema(t, f)
			admissionSQL(t, f, `UPDATE metadata SET value='8' WHERE key='schema:review_execution'`)
			if scenario == "dangling child" {
				conn, err := f.store.DB().Conn(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				for _, statement := range []string{`PRAGMA foreign_keys=OFF`, `UPDATE review_hosted_preflight SET attempt_id='missing-retained-attempt'`, `PRAGMA foreign_keys=ON`} {
					if _, err := conn.ExecContext(t.Context(), statement); err != nil {
						t.Fatal(err)
					}
				}
				conn.Close()
			} else {
				admissionSQL(t, f, `CREATE TRIGGER fail_timer_version BEFORE UPDATE ON metadata WHEN NEW.key='schema:review_execution' AND NEW.value='9' BEGIN SELECT RAISE(ABORT,'injected migration failure'); END`)
			}
			before := snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "review_execution_reservations") + snapshotTable(t, f, "review_execution_transitions") + snapshotTable(t, f, "review_hosted_preflight") + snapshotTable(t, f, "review_hosted_allowance_queries") + snapshotTable(t, f, "review_hosted_observations") + snapshotTable(t, f, "metadata")
			if err := EnsureSchema(t.Context(), f.store); err == nil {
				t.Fatal("invalid/injected migration committed")
			}
			after := snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "review_execution_reservations") + snapshotTable(t, f, "review_execution_transitions") + snapshotTable(t, f, "review_hosted_preflight") + snapshotTable(t, f, "review_hosted_allowance_queries") + snapshotTable(t, f, "review_hosted_observations") + snapshotTable(t, f, "metadata")
			if before != after {
				t.Fatal("failed migration changed retained bytes")
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM sqlite_master WHERE name IN ('review_operation_waits','review_execution_attempts_next','review_attempts_operation','review_attempts_live_operation')`) != 0 {
				t.Fatal("failed migration left new schema")
			}
			var ddl string
			if err := f.store.DB().QueryRow(`SELECT sql FROM sqlite_master WHERE name='review_execution_attempts'`).Scan(&ddl); err != nil || !strings.Contains(ddl, "UNIQUE") {
				t.Fatal("failed migration lost old attempt uniqueness", ddl, err)
			}
			var enabled int
			if err := f.store.DB().QueryRow(`PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 1 {
				t.Fatal("foreign key enforcement changed", enabled, err)
			}
		})
	}
}

func retainTimerCooldown(t *testing.T, f *attemptFixture, a Attempt, minutes time.Duration) HostedAllowanceInput {
	t.Helper()
	anchor := attemptAnchor(attemptProject, 2)
	q, created, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, reviewprovider.Author{ID: 12, Login: "poster", Kind: "User"}, reviewprovider.Author{ID: 30, Login: "coderabbitai[bot]", Kind: "Bot"})
	if err != nil || !created {
		t.Fatal("fresh query", err)
	}
	q, err = f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, hostedQueryComment(t, q))
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("c", 64)
	observed := stamp()
	start, _ := time.Parse(time.RFC3339Nano, q.EarliestPosting)
	in := HostedAllowanceInput{QueryID: q.ID, Anchor: anchor, ObservedAt: observed, ChangedFiles: 2, CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1234, Observation: reviewprovider.HostedAllowanceObservation{State: "cooldown_observed", Reason: "provider_cooldown_observed", QueryCommentID: q.Comment.ID, ResponseCommentID: 92, QueryCreatedAt: q.Comment.CreatedAt, ResponseCreatedAt: q.Comment.CreatedAt.Add(time.Nanosecond), RetryNotBefore: q.Comment.CreatedAt.Add(time.Nanosecond).Add(minutes)}}
	if _, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, HostedObservationInput{Kind: "allowance_query", CaptureID: in.CaptureID, CaptureSHA256: digest, CaptureBytes: in.CaptureBytes, StartedAt: start.Truncate(time.Second).Format(time.RFC3339Nano), FinishedAt: observed, ObservedAt: observed, ExpectedAnchor: anchor, Intent: q.Intent}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal(err)
	}
	return in
}

func TestCooldownMultipleWaitsPreserveOneRoundOccupancyAndReleaseReservations(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "hosted", 0, 0, 2, "grant")
	receipt := f.enqueue(t, attemptProject, "one-intent", "hosted", 2, 1)
	first := f.start(t, attemptProject, receipt)
	in := retainTimerCooldown(t, f, first, 3*time.Minute)
	if _, err := f.e.ScheduleHostedCooldown(t.Context(), attemptProject, first.ID, 1); err != nil {
		t.Fatal(err)
	}
	firstHistory := hostedAllowanceOnlyAttempt(t, f, first)
	now := in.Observation.RetryNotBefore
	f.e.schedulingNow = func() time.Time { return now }
	second := f.start(t, attemptProject, receipt)
	next := retainTimerCooldown(t, f, second, 6*time.Minute)
	if _, err := f.e.ScheduleHostedCooldown(t.Context(), attemptProject, second.ID, 1); err != nil {
		t.Fatal(err)
	}
	o := bindingOperation(t, f, f.scope(t, attemptProject, f.writer, false), receipt.ID)
	if o.Wait == nil || o.Wait.BaselineAttemptID != first.ID || o.Wait.RetryNotBefore != next.Observation.RetryNotBefore.Format(time.RFC3339Nano) {
		t.Fatal("repeated timer lost first comparison/latest deadline", o)
	}
	progress, err := f.e.ReadRoundProgressStages(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), []int64{2})
	if err != nil {
		t.Fatal(err)
	}
	c := progress.Items[0].Channels[0]
	if c.Pending != 1 || c.PendingExecution == nil || *c.PendingExecution != 1 || c.Used != 0 || c.Dry != 0 || c.Wait == nil {
		t.Fatal("cooldown changed round accounting", c)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
		t.Fatal("timer reserved capacity/credited review")
	}
	if hostedAllowanceOnlyAttempt(t, f, first) != firstHistory {
		t.Fatal("second timer rewrote initial retained observation")
	}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := ValidateOperationWaitsSnapshot(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	timerControl(t, f, receipt.ID, "cancel", "cancel")
	if c := readProgress(t, f, 2).Items[0].Channels[0]; c.Pending != 0 || c.Used != 0 {
		t.Fatal("cancel did not release pending round", c)
	}
}
func hostedAllowanceOnlyAttempt(t *testing.T, f *attemptFixture, a Attempt) string {
	t.Helper()
	var preflight, query string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_hosted_preflight WHERE attempt_id=?`, a.ID).Scan(&preflight); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_hosted_allowance_queries WHERE attempt_id=?`, a.ID).Scan(&query); err != nil {
		t.Fatal(err)
	}
	return preflight + query
}

func TestCooldownRecoveryPreservesOnlyProvenFuturePreparingWait(t *testing.T) {
	for _, scenario := range []string{"future", "elapsed", "unknown", "old held"} {
		t.Run(scenario, func(t *testing.T) {
			f, a, in := cooldownObservationFixture(t)
			if scenario != "unknown" {
				if err := f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, in); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "elapsed" {
				f.e.schedulingNow = func() time.Time { return in.Observation.RetryNotBefore }
			}
			if scenario == "old held" {
				if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, 1, "held", "hosted_allowance_cooldown", ""); err != nil {
					t.Fatal(err)
				}
			}
			before := hostedAllowanceOnlyAttemptOptional(t, f, a)
			if err := f.e.RecoverInterrupted(t.Context()); err != nil {
				t.Fatal(err)
			}
			o := bindingOperation(t, f, f.scope(t, attemptProject, f.writer, false), a.OperationID)
			if scenario == "future" {
				if o.State != "queued" || o.AttemptID != "" || o.Wait == nil {
					t.Fatal("preparation crash lost known timer", o)
				}
			} else if o.Wait != nil || o.State != "cancelled" && scenario != "old held" || scenario == "old held" && o.State != "held" {
				t.Fatal("unproven/elapsed/terminal work reopened", o)
			}
			if before != hostedAllowanceOnlyAttemptOptional(t, f, a) || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("recovery changed retained query/source or reserved capacity")
			}
			if err := f.e.RecoverInterrupted(t.Context()); err != nil {
				t.Fatal("recovery replay", err)
			}
		})
	}
}
func hostedAllowanceOnlyAttemptOptional(t *testing.T, f *attemptFixture, a Attempt) string {
	t.Helper()
	var result string
	if err := f.store.DB().QueryRow(`SELECT coalesce((SELECT body_json FROM review_hosted_preflight WHERE attempt_id=?),'')||coalesce((SELECT body_json FROM review_hosted_allowance_queries WHERE attempt_id=?),'')`, a.ID, a.ID).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCooldownRestoredProviderReleaseHoldsWaitAndRetainsHistory(t *testing.T) {
	f, a, in, _ := queuedCooldown(t)
	before := hostedAllowanceOnlyAttempt(t, f, a)
	restoredHeld(t, f)
	summary, err := restoredCall(t, f, time.Now())
	if err != nil || summary.QueuedOperations != 1 || summary.Attempts != 0 || summary.PausedProjects != 1 {
		t.Fatal("restore reconciliation", summary, err)
	}
	o := bindingOperation(t, f, f.scope(t, attemptProject, f.writer, false), a.OperationID)
	if o.State != "held" || o.Reason != "restore_reconciled" || o.Wait != nil || o.AttemptID != "" {
		t.Fatal("restore resumed pending intent", o)
	}
	f.e.schedulingNow = func() time.Time { return in.Observation.RetryNotBefore.Add(time.Hour) }
	pending, err := f.e.pending(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatal("restore scheduled wait", pending, err)
	}
	if before != hostedAllowanceOnlyAttempt(t, f, a) {
		t.Fatal("restore changed cooldown provenance")
	}
}

func TestCooldownRestoredProviderReleaseFreshAttemptAfterWaitAndControls(t *testing.T) {
	for _, scenario := range []string{"fresh preparing retry", "pause resume then prepare"} {
		t.Run(scenario, func(t *testing.T) {
			var f *attemptFixture
			var current Attempt
			var firstID string
			if scenario == "fresh preparing retry" {
				var in HostedAllowanceInput
				var now *time.Time
				f, current, in, now = queuedCooldown(t)
				firstID = current.ID
				*now = in.Observation.RetryNotBefore
				current = f.start(t, attemptProject, Receipt{ID: current.OperationID})
			} else {
				f = newAttemptFixture(t)
				f.configure(t, attemptProject, "test-capability", 0, false)
				receipt := f.enqueue(t, attemptProject, "pause-restore", "hosted", 2, 1)
				timerControl(t, f, receipt.ID, "pause", "pause")
				timerControl(t, f, receipt.ID, "resume", "resume")
				current = f.start(t, attemptProject, receipt)
			}
			restoredHeld(t, f)
			summary, err := restoredCall(t, f, time.Now())
			if err != nil || summary.Attempts != 1 || summary.QueuedOperations != 0 || summary.PausedProjects != 1 {
				t.Fatal("fresh revision reconciliation", summary, err)
			}
			o := bindingOperation(t, f, f.scope(t, attemptProject, f.writer, false), current.OperationID)
			if o.State != "cancelled" || o.Reason != "restore_reconciled" || o.AttemptID != current.ID || o.Wait != nil {
				t.Fatal("restored fresh attempt", o)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("restored retry reservation retained")
			}
			if firstID != "" {
				var state string
				if err := f.store.DB().QueryRow(`SELECT state FROM review_execution_attempts WHERE id=?`, firstID).Scan(&state); err != nil || state != "held" {
					t.Fatal("old cooldown changed", state, err)
				}
			}
		})
	}
}

func TestCooldownControlCASReplayAndAtomicEventFailure(t *testing.T) {
	f, a, _, _ := queuedCooldown(t)
	scope := f.scope(t, attemptProject, f.writer, false)
	o := bindingOperation(t, f, scope, a.OperationID)
	input := OperationControl{ExpectedRevision: o.Revision, Action: "pause"}
	admissionSQL(t, f, `CREATE TRIGGER fail_wait_control BEFORE INSERT ON events WHEN NEW.event_type='review.operation_updated' BEGIN SELECT RAISE(ABORT,'injected control failure'); END`)
	before := snapshotTable(t, f, "review_operation_waits") + snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "receipts")
	if _, err := f.e.ControlOperation(t.Context(), scope, "pause-key", o.ID, input); err == nil {
		t.Fatal("failed event accepted control")
	}
	if before != snapshotTable(t, f, "review_operation_waits")+snapshotTable(t, f, "review_operations")+snapshotTable(t, f, "receipts") {
		t.Fatal("failed control changed wait/request/receipt")
	}
	admissionSQL(t, f, `DROP TRIGGER fail_wait_control`)
	first, err := f.e.ControlOperation(t.Context(), scope, "pause-key", o.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	before = snapshotTable(t, f, "review_operation_waits") + snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "receipts") + snapshotTable(t, f, "events")
	replay, err := f.e.ControlOperation(t.Context(), scope, "pause-key", o.ID, input)
	if err != nil || string(first) != string(replay) {
		t.Fatal("control replay", err)
	}
	if before != snapshotTable(t, f, "review_operation_waits")+snapshotTable(t, f, "review_operations")+snapshotTable(t, f, "receipts")+snapshotTable(t, f, "events") {
		t.Fatal("control replay repeated mutation")
	}
	_, err = f.e.ControlOperation(t.Context(), scope, "stale-control", o.ID, input)
	attemptCode(t, err, "review_conflict")
	input.Action = "cancel"
	_, err = f.e.ControlOperation(t.Context(), scope, "pause-key", o.ID, input)
	attemptCode(t, err, "idempotency_conflict")
}

func TestCooldownDetachedWorkspacesRetireWithoutTouchingCurrentAttempt(t *testing.T) {
	f, a, in, now := queuedCooldown(t)
	later := time.Now().Add(10 * time.Minute)
	disposable, err := f.e.WorkspaceDisposable(t.Context(), "test-capability", a.ID, later)
	if err != nil || !disposable {
		t.Fatal("detached cooldown leaked workspace", disposable, err)
	}
	disposable, err = f.e.WorkspaceDisposable(t.Context(), "test-capability", a.ID, time.Now())
	if err != nil || disposable {
		t.Fatal("cooldown workspace retired too early", disposable, err)
	}
	*now = in.Observation.RetryNotBefore
	next := f.start(t, attemptProject, Receipt{ID: a.OperationID})
	disposable, err = f.e.WorkspaceDisposable(t.Context(), "test-capability", a.ID, later)
	if err != nil || !disposable {
		t.Fatal("new attempt hid old terminal workspace", disposable, err)
	}
	disposable, err = f.e.WorkspaceDisposable(t.Context(), "test-capability", next.ID, later)
	if err != nil || disposable {
		t.Fatal("current preparation workspace disposable", disposable, err)
	}
	admissionSQL(t, f, `DELETE FROM review_operation_waits WHERE operation_id=?`, a.OperationID)
	disposable, err = f.e.WorkspaceDisposable(t.Context(), "test-capability", a.ID, later)
	if err != nil || disposable {
		t.Fatal("unowned detached workspace disposable", disposable, err)
	}
}

func TestCooldownDetachedAttemptPublicationRepairsLedgerOnlyHistory(t *testing.T) {
	f := attemptProjectionFixture(t, false)
	first := f.start(t, attemptProject, f.enqueue(t, attemptProject, "ledger-only-cooldown", "hosted", 2, 1))
	in := retainTimerCooldown(t, f, first, 3*time.Minute)
	first, err := f.e.ScheduleHostedCooldown(t.Context(), attemptProject, first.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_operation_waits") + snapshotTable(t, f, "review_execution_transitions") + hostedAllowanceOnlyAttempt(t, f, first)
	pull := projectionPull(2)
	receipt, err := f.e.PublishAttempt(t.Context(), attemptProject, first.ID, &pull)
	if err != nil {
		t.Fatal("initial detached publication", err)
	}
	_, projected := projectedAttempt(t, f, first)
	assertProjectedTimes(t, f, first, projected, true)
	if projected.Reason != "hosted_allowance_cooldown" || projected.SourceRunID != "" || projected.TriggerID != "" || projected.State != "held" {
		t.Fatal("publication invented provider action/credit", projected)
	}
	digest := projectionDigest(t, f, "")
	events := attemptCount(t, f.store, `SELECT count(*) FROM events`)
	replay, err := f.e.PublishAttempt(t.Context(), attemptProject, first.ID, nil)
	if err != nil || receipt != replay || digest != projectionDigest(t, f, "") || events != attemptCount(t, f.store, `SELECT count(*) FROM events`) {
		t.Fatal("detached publication replay", replay, err)
	}
	if before != snapshotTable(t, f, "review_operations")+snapshotTable(t, f, "review_operation_waits")+snapshotTable(t, f, "review_execution_transitions")+hostedAllowanceOnlyAttempt(t, f, first) {
		t.Fatal("publication changed waiting authorization/history")
	}
	f.e.schedulingNow = func() time.Time { return in.Observation.RetryNotBefore }
	next := f.start(t, attemptProject, Receipt{ID: first.OperationID})
	if _, err := f.e.PublishAttempt(t.Context(), attemptProject, first.ID, nil); err != nil {
		t.Fatal("later current attempt blocked history repair", err)
	}
	_, live := projectedAttempt(t, f, next)
	if live.State != "preparing" {
		t.Fatal("historical publication changed live projection", live)
	}
	admissionSQL(t, f, `DELETE FROM review_operation_waits WHERE operation_id=?`, first.OperationID)
	_, err = f.e.PublishAttempt(t.Context(), attemptProject, first.ID, nil)
	attemptCode(t, err, "observation_conflict")
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
		t.Fatal("historical attempt projection created evidence")
	}
}
