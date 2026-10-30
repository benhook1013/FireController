package reviewexec

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// This controlled original action is known stopped by fixture construction.
// The public route records that independent assertion; it makes no provider call.
func pendingHostedSettlementFixture(t *testing.T) (*attemptFixture, Attempt, HostedObservationInput) {
	t.Helper()
	f, a, input := hostedObservationFixture(t)
	var err error
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "trigger_recorded", "101")
	if err != nil {
		t.Fatal(err)
	}
	input.StartedAt = stamp()
	input.FinishedAt = input.StartedAt
	input.ObservedAt = input.StartedAt
	if _, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	f.handler = mux
	return f, a, input
}

func TestHostedPendingSettlementActiveAndRestartPreserveOriginalAuthority(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint("restart=", restart), func(t *testing.T) {
			f, a, _ := pendingHostedSettlementFixture(t)
			if restart {
				if err := f.e.RecoverInterrupted(t.Context()); err != nil {
					t.Fatal(err)
				}
				a.State = "uncertain"
				a.Revision++
			}
			f.configure(t, otherAttemptProject, "other-account", 0, false)
			other := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "other-pending", "hosted", 4, 1))
			otherBefore := attemptState(t, f, other)
			// Original-work cleanup remains possible after current settings/catalog
			// changes and initiating actor revocation. Provider holds remain held.
			f.configure(t, attemptProject, "other-account", 1, true)
			admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
			admissionSQL(t, f, `INSERT INTO metadata(key,value) VALUES('restore:provider_state','held')`)
			var err error
			f.e, err = New(f.store, nil)
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			f.e.Register(mux)
			f.handler = mux
			before := transferAuthorityRows(t, f, true)
			observations, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !restart {
				admissionSQL(t, f, `CREATE TRIGGER pending_settlement_audit_failure BEFORE INSERT ON events WHEN NEW.event_type='review.hosted_settled' BEGIN SELECT RAISE(FAIL,'synthetic audit failure'); END`)
				original := hostedObservationState(t, f, a)
				failed := hostedSettlementCall(t, f, a, "verified-stopped-pending", hostedSettlementBody(a))
				if failed.Code != 500 || hostedObservationState(t, f, a) != original {
					t.Fatal("active settlement audit failure did not roll back", failed.Code)
				}
				admissionSQL(t, f, `DROP TRIGGER pending_settlement_audit_failure`)
			}
			r := hostedSettlementCall(t, f, a, "verified-stopped-pending", hostedSettlementBody(a))
			if r.Code != 200 {
				t.Fatal(r.Code, r.Body.String())
			}
			var receipt attemptSettlementReceipt
			if json.Unmarshal(r.Body.Bytes(), &receipt) != nil || receipt.State != "cancelled" || receipt.AttemptID != a.ID || receipt.OperationID != a.OperationID || receipt.Revision != a.Revision+1 || receipt.Outcome != "settled" {
				t.Fatal(r.Body.String())
			}
			after := transferAuthorityRows(t, f, true)
			for _, table := range []string{"review_execution_evidence", "review_hosted_observations", "review_adjudication_state", "review_adjudication_changes", "review_finding_routes", "review_finding_route_revisions", "review_execution_config", "review_round_controls", "review_round_changes", "review_round_completions"} {
				if !reflect.DeepEqual(before[table], after[table]) {
					t.Fatal("settlement changed retained source/accounting", table)
				}
			}
			if attemptState(t, f, other) != otherBefore || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE state='completed'`) != 0 {
				t.Fatal("settlement changed another reservation or supplied credit")
			}
			var state, reason, intent, hold string
			if err = f.store.DB().QueryRow(`SELECT o.state,o.reason,a.intent FROM review_operations o JOIN review_execution_attempts a ON a.id=o.attempt_id WHERE a.id=?`, a.ID).Scan(&state, &reason, &intent); err != nil || state != "cancelled" || reason != "operator_verified_external_stop" || intent != a.Intent {
				t.Fatal(state, reason, intent, err)
			}
			if err = f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='restore:provider_state'`).Scan(&hold); err != nil || hold != "held" {
				t.Fatal("settlement released provider hold", err)
			}
			retained, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
			if err != nil || !reflect.DeepEqual(observations, retained) {
				t.Fatal("original references changed", err)
			}
			var auditBody, actor string
			if err = f.store.DB().QueryRow(`SELECT payload_json,actor_id FROM events WHERE event_type='review.hosted_settled' AND resource_id=?`, a.ID).Scan(&auditBody, &actor); err != nil {
				t.Fatal(err)
			}
			var audit attemptSettlementAudit
			if json.Unmarshal([]byte(auditBody), &audit) != nil || !audit.ExternalActionStopped || audit.ExpectedRevision != a.Revision || audit.ExternalID != a.ExternalID || audit.Anchor != *a.Anchor || actor != f.scope(t, attemptProject, f.admin, true).PrincipalID {
				t.Fatal("missing original administrator assertion", auditBody)
			}
			settled := hostedObservationState(t, f, a)
			replay := hostedSettlementCall(t, f, a, "verified-stopped-pending", hostedSettlementBody(a))
			if replay.Code != 200 || replay.Body.String() != r.Body.String() || hostedObservationState(t, f, a) != settled {
				t.Fatal("exact replay changed state", replay.Body.String())
			}
		})
	}
}

func TestHostedPendingSettlementRejectsUnprovenBinding(t *testing.T) {
	f, a, input := pendingHostedSettlementFixture(t)
	var err error
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "uncertain", "supervisor_interrupted", "")
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "trigger_recovered", "")
	if err != nil {
		t.Fatal(err)
	}
	var originalObservation string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_hosted_observations WHERE attempt_id=?`, a.ID).Scan(&originalObservation); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{f.writer, f.reader} {
		settlementError(t, f.request("POST", hostedSettlementPath(attemptProject, a), token, "deny-nonadmin", encoded(hostedSettlementBody(a))), 403, "forbidden")
	}
	// Each independent change is rolled back through exact fixture values so the
	// matrix reuses one database without rebuilding unrelated schema per guard.
	for _, tc := range []struct {
		name, mutate, restore string
		args, undo            []any
	}{
		{"unconfirmed-reason", `UPDATE review_execution_transitions SET reason='unconfirmed_action' WHERE attempt_id=? AND revision=3`, `UPDATE review_execution_transitions SET reason='trigger_recorded' WHERE attempt_id=? AND revision=3`, []any{a.ID}, []any{a.ID}},
		{"confirmed-external-mismatch", `UPDATE review_execution_transitions SET external_id='102' WHERE attempt_id=? AND revision=3`, `UPDATE review_execution_transitions SET external_id='101' WHERE attempt_id=? AND state='active'`, []any{a.ID}, []any{a.ID}},
		{"foreign-observation", `UPDATE review_hosted_observations SET body_json=json_set(body_json,'$.operation_id','review_operation_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee') WHERE attempt_id=?`, `UPDATE review_hosted_observations SET body_json=? WHERE attempt_id=?`, []any{a.ID}, []any{originalObservation, a.ID}},
		{"incomplete-observation", `UPDATE review_hosted_observations SET body_json=json_set(body_json,'$.input.capture_bytes',0) WHERE attempt_id=?`, `UPDATE review_hosted_observations SET body_json=? WHERE attempt_id=?`, []any{a.ID}, []any{originalObservation, a.ID}},
		{"reservation-account", `UPDATE review_execution_reservations SET account='foreign_account' WHERE attempt_id=?`, `UPDATE review_execution_reservations SET account=? WHERE attempt_id=?`, []any{a.ID}, []any{a.Account, a.ID}},
		{"reservation-target", `UPDATE review_execution_reservations SET pull_number=4 WHERE attempt_id=?`, `UPDATE review_execution_reservations SET pull_number=2 WHERE attempt_id=?`, []any{a.ID}, []any{a.ID}},
		{"invalid-original-config", `UPDATE review_execution_config SET body_json=body_json||' ' WHERE project_id=? AND revision=1`, `UPDATE review_execution_config SET body_json=rtrim(body_json) WHERE project_id=? AND revision=1`, []any{attemptProject}, []any{attemptProject}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admissionSQL(t, f, tc.mutate, tc.args...)
			before := hostedObservationState(t, f, a)
			settlementError(t, hostedSettlementCall(t, f, a, "deny-"+tc.name, hostedSettlementBody(a)), 409, "review_settlement_unavailable")
			if hostedObservationState(t, f, a) != before {
				t.Fatal("refusal wrote state")
			}
			admissionSQL(t, f, tc.restore, tc.undo...)
		})
	}
	for _, external := range []string{"", "0", "0101", "-101", "review-query"} {
		t.Run("external="+external, func(t *testing.T) {
			admissionSQL(t, f, `UPDATE review_execution_attempts SET external_id=? WHERE id=?`, external, a.ID)
			admissionSQL(t, f, `UPDATE review_execution_transitions SET external_id=? WHERE attempt_id=? AND state='active'`, external, a.ID)
			copy := a
			copy.ExternalID = external
			settlementError(t, hostedSettlementCall(t, f, copy, "deny-external-"+strings.ReplaceAll(external, "-", "minus"), hostedSettlementBody(copy)), 409, "review_settlement_unavailable")
			admissionSQL(t, f, `UPDATE review_execution_attempts SET external_id='101' WHERE id=?`, a.ID)
			admissionSQL(t, f, `UPDATE review_execution_transitions SET external_id='101' WHERE attempt_id=? AND state='active'`, a.ID)
		})
	}
	// The administrator assertion and original exact target remain mandatory.
	for _, change := range []map[string]any{{"external_action_stopped": false}, {"expected_head_sha": strings.Repeat("f", 40)}, {"expected_repository_id": 802}, {"expected_external_id": "102"}} {
		body := hostedSettlementBody(a)
		for k, v := range change {
			body[k] = v
		}
		code, status := "review_settlement_unavailable", 409
		if body["external_action_stopped"] == false {
			code, status = "validation_failed", 400
		}
		settlementError(t, hostedSettlementCall(t, f, a, "deny-request", body), status, code)
	}
	// Missing observations and a prior allowance query cannot substitute for an
	// ordinary bound review observation. Restore exact original bytes afterward.
	var observationBody string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_hosted_observations WHERE attempt_id=?`, a.ID).Scan(&observationBody); err != nil {
		t.Fatal(err)
	}
	var stored HostedObservation
	if err := json.Unmarshal([]byte(observationBody), &stored); err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `DELETE FROM review_hosted_observations WHERE attempt_id=?`, a.ID)
	settlementError(t, hostedSettlementCall(t, f, a, "deny-no-observation", hostedSettlementBody(a)), 409, "review_settlement_unavailable")
	admissionSQL(t, f, `INSERT INTO review_hosted_observations(id,project_id,attempt_id,operation_id,position,capture_sha256,capture_bytes,body_json) VALUES(?,?,?,?,?,?,?,?)`, stored.ID, stored.ProjectID, stored.AttemptID, stored.OperationID, stored.Position, stored.Input.CaptureSHA256, stored.Input.CaptureBytes, observationBody)
	admissionSQL(t, f, `UPDATE review_hosted_observations SET body_json=json_set(body_json,'$.input.kind','allowance_query') WHERE attempt_id=?`, a.ID)
	settlementError(t, hostedSettlementCall(t, f, a, "deny-query-only", hostedSettlementBody(a)), 409, "review_settlement_unavailable")
	admissionSQL(t, f, `UPDATE review_hosted_observations SET body_json=? WHERE attempt_id=?`, observationBody, a.ID)
	// Final evidence belongs to normal settlement and cannot be abandoned here.
	if _, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, hostedSettlementFinalInput(a, input)); err != nil {
		t.Fatal(err)
	}
	settlementError(t, hostedSettlementCall(t, f, a, "deny-final-evidence", hostedSettlementBody(a)), 409, "review_settlement_unavailable")
}

func TestHostedPendingSettlementConcurrentCompletionAndAdmin(t *testing.T) {
	f, a, input := pendingHostedSettlementFixture(t)
	start := make(chan struct{})
	captured := make(chan error, 1)
	settled := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		<-start
		_, err := f.e.RecordReviewEvidence(context.Background(), attemptProject, a.ID, hostedSettlementFinalInput(a, input))
		captured <- err
	}()
	go func() {
		<-start
		settled <- hostedSettlementCall(t, f, a, "pending-completion-race", hostedSettlementBody(a))
	}()
	close(start)
	err, r := <-captured, <-settled
	if r.Code == 200 {
		attemptCode(t, err, "review_conflict")
		if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, a.ID) != 0 {
			t.Fatal("late capture credited settled attempt")
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		settlementError(t, r, 409, "review_settlement_unavailable")
		if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 1 {
			t.Fatal("completion race released reservation")
		}
	}
	// A current admin is required even for exact receipt replay.
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE is_admin=1`)
	settlementError(t, hostedSettlementCall(t, f, a, "pending-completion-race", hostedSettlementBody(a)), 401, "unauthorized")
}
