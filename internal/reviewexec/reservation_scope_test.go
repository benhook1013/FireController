package reviewexec

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestReservationScopeHostedPerRepositoryAndPending(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	first := hostedAdmissionAttempt(t, f, "hosted-first-repo", "hosted", 1, attemptAnchor(attemptProject, 2))
	var err error
	first, err = f.e.Advance(t.Context(), attemptProject, first.ID, first.Revision, "active", "posted", "101")
	if err != nil {
		t.Fatal(err)
	}
	before := attemptState(t, f, first)
	blocked := f.enqueue(t, attemptProject, "hosted-same-repo", "hosted", 4, 1)
	independent := f.enqueue(t, otherAttemptProject, "hosted-other-repo", "hosted", 2, 1)
	pending, err := f.e.pending(t.Context())
	if err != nil || len(pending) != 1 || pending[0].ID != independent.ID {
		t.Fatalf("other repository hidden by account reservation: %+v %v", pending, err)
	}
	_, err = f.e.StartAttempt(t.Context(), attemptProject, blocked.ID)
	attemptCode(t, err, "review_busy")
	second := f.start(t, otherAttemptProject, independent)
	anchor := attemptAnchor(otherAttemptProject, 2)
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: second.OperationID, AttemptID: second.ID, HeadSHA: anchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	second, err = f.e.Admit(t.Context(), otherAttemptProject, second.ID, second.Revision, anchor, intent)
	if err != nil || second.State != "posting" {
		t.Fatal("independent Hosted admission", second, err)
	}
	if before != attemptState(t, f, first) || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 2 {
		t.Fatal("original active reservation changed")
	}
	// Physical constraints independently enforce both scopes, not just the
	// transaction's precheck. Failed writes preserve the full original rows.
	reservations := snapshotTable(t, f, "review_execution_reservations")
	for _, statement := range []string{
		`UPDATE review_execution_reservations SET repository_id=801,pull_number=4 WHERE attempt_id=?`,
		`UPDATE review_execution_reservations SET repository_id=801,account='different-account' WHERE attempt_id=?`,
	} {
		if _, err := f.store.DB().Exec(statement, second.ID); err == nil {
			t.Fatal("reservation constraint accepted duplicate capacity or target")
		}
		if snapshotTable(t, f, "review_execution_reservations") != reservations {
			t.Fatal("constraint failure changed reservations")
		}
	}
	// A different account cannot acquire the same external target.
	f.configure(t, attemptProject, "other-account", 1, false)
	_, err = f.e.Enqueue(t.Context(), f.scope(t, attemptProject, f.writer, false), "hosted-exact-target-other-account", Request{ExpectedConfigurationRevision: 2, PullNumber: 2, Channel: "hosted", HeadSHA: strings.Repeat("a", 40)})
	requirePendingRequest(t, err, first.OperationID)
	// A separate account can use another target alongside the second repo's
	// reservation, without changing the first project's retained queued order.
	f.configure(t, otherAttemptProject, "other-account", 1, false)
	f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "hosted-other-account-other-target", "hosted", 4, 2))
}

func TestReservationScopeCLIStillGlobalAcrossRepositories(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	first := f.start(t, attemptProject, f.enqueue(t, attemptProject, "cli-first-repo", "cli", 2, 1))
	queued := f.enqueue(t, otherAttemptProject, "cli-other-repo", "cli", 4, 1)
	before := attemptState(t, f, first)
	_, err := f.e.StartAttempt(t.Context(), otherAttemptProject, queued.ID)
	attemptCode(t, err, "review_busy")
	pending, err := f.e.pending(t.Context())
	if err != nil || len(pending) != 0 || before != attemptState(t, f, first) {
		t.Fatal("CLI account scope changed", pending, err)
	}
	f.configure(t, otherAttemptProject, "other-account", 1, false)
	otherScope := f.scope(t, otherAttemptProject, f.other, false)
	original := bindingOperation(t, f, otherScope, queued.ID)
	if _, err := f.e.ControlOperation(t.Context(), otherScope, "replace-original-account-intent", queued.ID, OperationControl{ExpectedRevision: original.Revision, Action: "cancel"}); err != nil {
		t.Fatal(err)
	}
	second := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "cli-other-account", "cli", 4, 2))
	beforeRows := snapshotTable(t, f, "review_execution_reservations")
	if _, err := f.store.DB().Exec(`UPDATE review_execution_reservations SET account=? WHERE attempt_id=?`, first.Account, second.ID); err == nil || snapshotTable(t, f, "review_execution_reservations") != beforeRows {
		t.Fatal("CLI physical account constraint weakened", err)
	}
}

// Rebuild only this disposable fixture's reservation table as the real v9 DDL.
func reservationScopeRestoreV9(t *testing.T, f *attemptFixture) {
	t.Helper()
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		for _, statement := range []string{
			`CREATE TABLE old_reservations(provider TEXT NOT NULL,repository_id INTEGER NOT NULL,pull_number INTEGER NOT NULL,action_class TEXT NOT NULL,account TEXT NOT NULL,attempt_id TEXT NOT NULL UNIQUE REFERENCES review_execution_attempts(id),PRIMARY KEY(provider,repository_id,pull_number,action_class),UNIQUE(provider,account,action_class))`,
			`INSERT INTO old_reservations SELECT * FROM review_execution_reservations`,
			`DROP TABLE review_execution_reservations`,
			`ALTER TABLE old_reservations RENAME TO review_execution_reservations`,
			`UPDATE metadata SET value='9' WHERE key='schema:review_execution'`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestReservationScopeMigrationPreservesActiveAndQueued(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	a := hostedAdmissionAttempt(t, f, "migration-hosted-active", "hosted", 1, attemptAnchor(attemptProject, 2))
	var err error
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "posted", "101")
	if err != nil {
		t.Fatal(err)
	}
	cli := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "migration-cli-active", "cli", 4, 1))
	if _, err = f.e.Admit(t.Context(), otherAttemptProject, cli.ID, cli.Revision, attemptAnchor(otherAttemptProject, 4), ""); err != nil {
		t.Fatal(err)
	}
	queued := f.enqueue(t, otherAttemptProject, "migration-queued-hosted", "hosted", 2, 1)
	control := func(action, key string) {
		scope := f.scope(t, otherAttemptProject, f.other, false)
		o := bindingOperation(t, f, scope, queued.ID)
		if _, err := f.e.ControlOperation(t.Context(), scope, key, queued.ID, OperationControl{ExpectedRevision: o.Revision, Action: action}); err != nil {
			t.Fatal(err)
		}
	}
	control("pause", "migration-pause")
	reservationScopeRestoreV9(t, f)
	tables := []string{"review_execution_config", "review_operations", "review_operation_waits", "review_execution_attempts", "review_execution_reservations", "review_execution_transitions", "review_hosted_preflight", "review_hosted_allowance_queries", "review_hosted_observations", "review_execution_evidence", "receipts", "events"}
	snapshot := func() map[string]string {
		result := map[string]string{}
		for _, table := range tables {
			result[table] = snapshotTable(t, f, table)
		}
		return result
	}
	before := snapshot()
	var oldDDL string
	if err = f.store.DB().QueryRow(`SELECT sql FROM sqlite_master WHERE name='review_execution_reservations'`).Scan(&oldDDL); err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `CREATE TRIGGER fail_reservation_version BEFORE UPDATE ON metadata WHEN NEW.key='schema:review_execution' AND NEW.value='10' BEGIN SELECT RAISE(ABORT,'injected migration failure'); END`)
	if err = EnsureSchema(t.Context(), f.store); err == nil {
		t.Fatal("injected migration succeeded")
	}
	var ddl, version string
	if err = f.store.DB().QueryRow(`SELECT sql FROM sqlite_master WHERE name='review_execution_reservations'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if err = f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_execution'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if ddl != oldDDL || version != "9" || !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("failed migration changed original schema or retained data")
	}
	admissionSQL(t, f, `DROP TRIGGER fail_reservation_version`)
	if err = EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if err = f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_execution'`).Scan(&version); err != nil || version != "10" {
		t.Fatal(version, err)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("migration rewrote retained reservations/anchors/queued work")
	}
	if err = validateExecutionForeignKeysTest(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	if err = EnsureSchema(t.Context(), f.store); err != nil || !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("migration not idempotent", err)
	}
	// Original paused work stays paused, then resumes with the same identity.
	paused, err := f.e.StartAttempt(t.Context(), otherAttemptProject, queued.ID)
	if err != nil || paused.ID != "" {
		t.Fatal("migration lost queued pause", paused, err)
	}
	control("resume", "migration-resume")
	// The exact queued operation, not a replacement request, now owns its repo slot.
	f.start(t, otherAttemptProject, queued)
	if before["review_execution_reservations"] == snapshotTable(t, f, "review_execution_reservations") {
		t.Fatal("queued work did not acquire independent Hosted reservation")
	}
}
