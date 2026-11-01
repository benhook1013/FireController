package reviewexec

import (
	"reflect"
	"testing"
)

func confirmedHostedRestartFixture(t *testing.T) (*attemptFixture, Attempt) {
	t.Helper()
	f, a, _ := hostedRecoveryFixture(t)
	var err error
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "trigger_posted", "101")
	if err != nil {
		t.Fatal(err)
	}
	return f, a
}

func TestHostedRestartConfirmedTriggerKeepsWaitingAndOriginalAuthority(t *testing.T) {
	f, a := confirmedHostedRestartFixture(t)
	original, err := f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "test-capability")
	if err != nil {
		t.Fatal(err)
	}
	var prefix string
	if err = f.store.DB().QueryRow(`SELECT group_concat(revision||'/'||state||'/'||reason||'/'||external_id||'/'||created_at,'|') FROM review_execution_transitions WHERE attempt_id=? ORDER BY revision`, a.ID).Scan(&prefix); err != nil {
		t.Fatal(err)
	}
	// Admission changes do not invalidate an already issued external action.
	f.configure(t, attemptProject, "other-account", 1, true)
	rounds := roundTables(t, f) // retain new configuration audit too
	cliAllowanceReopen(t, f)
	if err = f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "test-capability")
	if err != nil || after.Attempt.State != "active" || after.Operation.Reason != "supervisor_resumed" || after.Attempt.Revision != a.Revision+1 || after.Operation.Revision != original.Operation.Revision+1 || after.Attempt.Intent != a.Intent || after.Attempt.ExternalID != a.ExternalID || !reflect.DeepEqual(after.Attempt.Anchor, a.Anchor) || after.EarliestPosting != original.EarliestPosting || after.EvidenceID != "" {
		t.Fatal("restart changed issued review identity", after, err)
	}
	var retained string
	if err = f.store.DB().QueryRow(`SELECT group_concat(revision||'/'||state||'/'||reason||'/'||external_id||'/'||created_at,'|') FROM review_execution_transitions WHERE attempt_id=? AND revision<=? ORDER BY revision`, a.ID, a.Revision).Scan(&retained); err != nil || retained != prefix {
		t.Fatal("restart rewrote original journal", retained, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=? AND provider=? AND account=? AND action_class='hosted' AND repository_id=? AND pull_number=?`, a.ID, a.Provider, a.Account, a.Anchor.RepositoryID, a.Anchor.PullNumber) != 1 || rounds != roundTables(t, f) || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
		t.Fatal("restart changed reservation or review credit")
	}
	status, err := f.e.ReadOperationStatus(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), a.OperationID)
	if err != nil || status.State != "active" || status.Stage != "provider_wait" || status.NextAction != "wait" {
		t.Fatal("restart asks agent to intervene", status, err)
	}
	target, err := singleHostedRecovery(t, f.e, t.Context(), "test-capability")
	if err != nil || target == nil || target.AttemptID != a.ID || target.OperationID != a.OperationID {
		t.Fatal("restart stranded observer", target, err)
	}
	before := hostedObservationState(t, f, a)
	for range 2 {
		if err = f.e.RecoverInterrupted(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if before != hostedObservationState(t, f, a) || rounds != roundTables(t, f) {
		t.Fatal("repeated startup duplicated handoff or credit")
	}
}

func TestHostedRestartUnconfirmedAndCorruptProofStayFailClosed(t *testing.T) {
	cases := map[string]string{
		"missing-confirmation":        `DELETE FROM review_execution_transitions WHERE attempt_id=? AND revision=3`,
		"unconfirmed-reason":          `UPDATE review_execution_transitions SET reason='unproven' WHERE attempt_id=? AND revision=3`,
		"different-confirmed-trigger": `UPDATE review_execution_transitions SET external_id='102' WHERE attempt_id=? AND revision=3`,
		"invalid-trigger-id":          `UPDATE review_execution_attempts SET external_id='0101' WHERE id=?`,
		"missing-reservation":         `DELETE FROM review_execution_reservations WHERE attempt_id=?`,
		"foreign-reservation":         `UPDATE review_execution_reservations SET account='foreign' WHERE attempt_id=?`,
		"foreign-target":              `UPDATE review_execution_reservations SET pull_number=4 WHERE attempt_id=?`,
		"bad-intent":                  `UPDATE review_execution_attempts SET intent='unproven' WHERE id=?`,
		"bad-current-reason":          `UPDATE review_operations SET reason='unproven' WHERE attempt_id=?`,
		"invalid-original-config":     `UPDATE review_execution_config SET body_json=body_json||' ' WHERE revision=1 AND project_id=(SELECT project_id FROM review_execution_attempts WHERE id=?)`,
		"bad-admission":               `UPDATE review_execution_transitions SET reason='unproven' WHERE attempt_id=? AND revision=2`,
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			f, a := confirmedHostedRestartFixture(t)
			admissionSQL(t, f, damage, a.ID)
			before := hostedObservationState(t, f, a)
			err := f.e.RecoverInterrupted(t.Context())
			var state string
			if readErr := f.store.DB().QueryRow(`SELECT state FROM review_execution_attempts WHERE id=?`, a.ID).Scan(&state); readErr != nil {
				t.Fatal(readErr)
			}
			if err != nil {
				if before != hostedObservationState(t, f, a) {
					t.Fatal("refusal partly changed journal", err)
				}
			} else if state != "uncertain" {
				t.Fatal("unproven trigger survived restart", state)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions WHERE attempt_id=? AND reason='supervisor_resumed'`, a.ID) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions`) != 0 {
				t.Fatal("unproven restart gained handoff or review credit")
			}
		})
	}
	for _, channel := range []string{"posting", "cli"} {
		t.Run(channel, func(t *testing.T) {
			f, a, _ := hostedRecoveryFixture(t)
			if channel == "cli" {
				f, a, _ = evidenceFixture(t, "cli", true)
			}
			if err := f.e.RecoverInterrupted(t.Context()); err != nil {
				t.Fatal(err)
			}
			var state string
			if err := f.store.DB().QueryRow(`SELECT state FROM review_execution_attempts WHERE id=?`, a.ID).Scan(&state); err != nil || state != "uncertain" {
				t.Fatal("possibly executing local/posting action treated as confirmed Hosted", state, err)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
				t.Fatal("unsafe restart released reservation or granted credit")
			}
		})
	}
}

func TestHostedRestartExistingObserverHandoffStillRequiresProof(t *testing.T) {
	for _, reason := range []string{"trigger_recovered", "supervisor_resumed"} {
		t.Run(reason, func(t *testing.T) {
			f, a, _ := hostedRecoveryFixture(t)
			var err error
			a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "trigger_recovered", "101")
			if err != nil {
				t.Fatal(err)
			}
			if reason == "supervisor_resumed" {
				// Construct the same durable handoff through normal startup.
				admissionSQL(t, f, `UPDATE review_execution_transitions SET reason='trigger_posted' WHERE attempt_id=? AND revision=3`, a.ID)
				admissionSQL(t, f, `UPDATE review_operations SET reason='trigger_posted' WHERE id=?`, a.OperationID)
				if err = f.e.RecoverInterrupted(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			before := hostedObservationState(t, f, a)
			if err = f.e.RecoverInterrupted(t.Context()); err != nil || before != hostedObservationState(t, f, a) {
				t.Fatal("existing observer handoff changed", err)
			}
			// The idempotent shortcut must not bypass original reservation proof.
			admissionSQL(t, f, `UPDATE review_execution_reservations SET account='foreign' WHERE attempt_id=?`, a.ID)
			if err = f.e.RecoverInterrupted(t.Context()); err != nil {
				t.Fatal(err)
			}
			var state string
			if err = f.store.DB().QueryRow(`SELECT state FROM review_execution_attempts WHERE id=?`, a.ID).Scan(&state); err != nil || state != "uncertain" {
				t.Fatal("unproven existing handoff remained active", state, err)
			}
		})
	}
}
