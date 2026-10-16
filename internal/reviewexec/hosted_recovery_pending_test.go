package reviewexec

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func pendingRecoveryAdvance(t *testing.T, f *attemptFixture, a Attempt, state, reason, external string) Attempt {
	t.Helper()
	updated, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, state, reason, external)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func TestPendingHostedRecoverySelectsInterruptedAndAdoptedOnly(t *testing.T) {
	for _, state := range []string{"posting", "uncertain", "live-active", "recovered-active", "stale-recovered-transition", "completed", "failed", "cancelled", "rate_limited", "preparing"} {
		t.Run(state, func(t *testing.T) {
			f, a, _ := hostedRecoveryFixture(t)
			switch state {
			case "uncertain":
				if err := f.e.RecoverInterrupted(t.Context()); err != nil {
					t.Fatal(err)
				}
			case "live-active", "completed", "cancelled":
				a = pendingRecoveryAdvance(t, f, a, "active", "trigger_recorded", "101")
				if state != "live-active" {
					a = pendingRecoveryAdvance(t, f, a, state, "capture_settled", "")
				}
			case "recovered-active", "stale-recovered-transition":
				a = pendingRecoveryAdvance(t, f, a, "uncertain", "interrupted", "")
				a = pendingRecoveryAdvance(t, f, a, "active", "trigger_recovered", "101")
				if state == "stale-recovered-transition" {
					a = pendingRecoveryAdvance(t, f, a, "uncertain", "again_interrupted", "")
					a = pendingRecoveryAdvance(t, f, a, "active", "trigger_recorded", "")
				}
			case "failed", "rate_limited":
				a = pendingRecoveryAdvance(t, f, a, state, "not_completed", "")
			case "preparing":
				admissionSQL(t, f, `UPDATE review_execution_attempts SET state='preparing' WHERE id=?`, a.ID)
				admissionSQL(t, f, `UPDATE review_operations SET state='preparing' WHERE id=?`, a.OperationID)
			}
			before := hostedObservationState(t, f, a)
			candidate, err := f.e.PendingHostedRecovery(t.Context(), "test-capability")
			eligible := state == "uncertain" || state == "recovered-active"
			if err != nil || (candidate != nil) != eligible {
				t.Fatal("wrong observation ownership selection", state, candidate, err)
			}
			if candidate != nil {
				if candidate.ProjectID != attemptProject || candidate.AttemptID != a.ID {
					t.Fatal("selected another target", candidate)
				}
				// Selection is not a substitute for strict recovery input loading.
				if _, err := f.e.ReadHostedRecovery(t.Context(), candidate.ProjectID, candidate.AttemptID, "test-capability"); err != nil {
					t.Fatal("selected original binding cannot be read", err)
				}
				candidate.ProjectID = "caller-owned-copy"
				again, err := f.e.PendingHostedRecovery(t.Context(), "test-capability")
				if err != nil || again.ProjectID != attemptProject {
					t.Fatal("caller changed retained candidate", err)
				}
			}
			if hostedObservationState(t, f, a) != before {
				t.Fatal("selector mutated journal/evidence/events/receipts")
			}
		})
	}
}

func TestPendingHostedRecoveryRetainsOriginalConfigAfterPauseRevocationAndRestart(t *testing.T) {
	f, a, _ := hostedRecoveryFixture(t)
	a = pendingRecoveryAdvance(t, f, a, "uncertain", "interrupted", "")
	f.configure(t, attemptProject, "other-account", 1, true)
	admissionPolicy(t, f, 1, "hosted")
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
	admissionSQL(t, f, `DELETE FROM grants WHERE credential_id='exec-writer-cred'`)
	cliAllowanceReopen(t, f)
	var err error
	f.e, err = New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	before := hostedObservationState(t, f, a)
	got, err := f.e.PendingHostedRecovery(t.Context(), "test-capability")
	if err != nil || !reflect.DeepEqual(got, &HostedRecoveryCandidate{ProjectID: attemptProject, AttemptID: a.ID}) {
		t.Fatal("latest admission settings erased issued observation", got, err)
	}
	other, err := f.e.PendingHostedRecovery(t.Context(), "other-account")
	if err != nil || other != nil {
		t.Fatal("latest capability adopted another original account", other, err)
	}
	if hostedObservationState(t, f, a) != before {
		t.Fatal("original binding selection mutated state")
	}
}

func TestPendingHostedRecoverySeparatesProjectAccountChannelAndFingerprint(t *testing.T) {
	f, a, _ := hostedRecoveryFixture(t)
	a = pendingRecoveryAdvance(t, f, a, "uncertain", "interrupted", "")
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	other := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "other-issued", "hosted", 2, 1))
	other = pendingRecoveryAdvance(t, f, other, "held", "fixture_stop", "")
	// A queued operation on another project/account must not be selected.
	r := f.enqueue(t, otherAttemptProject, "other-queued", "hosted", 4, 1)
	got, err := f.e.PendingHostedRecovery(t.Context(), "other-account")
	if err != nil || got != nil {
		t.Fatal("unissued work selected", got, err)
	}
	other = f.start(t, otherAttemptProject, r)
	anchor := attemptAnchor(otherAttemptProject, 4)
	body, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: other.OperationID, AttemptID: other.ID, HeadSHA: anchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	other, err = f.e.Admit(t.Context(), otherAttemptProject, other.ID, other.Revision, anchor, body)
	if err != nil {
		t.Fatal(err)
	}
	other = pendingRecoveryAdvance(t, f, other, "uncertain", "interrupted", "")
	for capability, expected := range map[string]*HostedRecoveryCandidate{
		"test-capability": {ProjectID: attemptProject, AttemptID: a.ID},
		"other-account":   {ProjectID: otherAttemptProject, AttemptID: other.ID},
	} {
		selected, err := f.e.PendingHostedRecovery(t.Context(), capability)
		if err != nil || !reflect.DeepEqual(selected, expected) {
			t.Fatal("account/project selection crossed original binding", selected, err)
		}
	}
	for _, change := range []string{"fingerprint", "account", "provider", "channel", "original-capability", "operation-state", "operation-attempt", "attempt-project"} {
		t.Run(change, func(t *testing.T) {
			f, a, _ := hostedRecoveryFixture(t)
			a = pendingRecoveryAdvance(t, f, a, "uncertain", "interrupted", "")
			switch change {
			case "fingerprint", "account", "provider":
				caps := append([]Capability(nil), attemptCapabilities...)
				if change == "fingerprint" {
					caps[0].Fingerprint = strings.Repeat("f", 64)
				} else if change == "account" {
					caps[0].Account = "another-account"
				} else {
					caps[0].Provider = "another-provider"
				}
				f.e, err = New(f.store, caps)
				if err != nil {
					t.Fatal(err)
				}
			case "channel":
				admissionSQL(t, f, `UPDATE review_operations SET channel='cli' WHERE id=?`, a.OperationID)
			case "original-capability":
				admissionSQL(t, f, `UPDATE review_execution_config SET body_json=json_set(body_json,'$.capability_id','other-account') WHERE project_id=? AND revision=1`, attemptProject)
			case "operation-state":
				admissionSQL(t, f, `UPDATE review_operations SET state='active' WHERE id=?`, a.OperationID)
			case "operation-attempt":
				admissionSQL(t, f, `UPDATE review_operations SET attempt_id='' WHERE id=?`, a.OperationID)
			case "attempt-project":
				admissionSQL(t, f, `UPDATE review_execution_attempts SET project_id=? WHERE id=?`, otherAttemptProject, a.ID)
			}
			before := hostedObservationState(t, f, a)
			got, err := f.e.PendingHostedRecovery(t.Context(), "test-capability")
			if err != nil || got != nil || hostedObservationState(t, f, a) != before {
				t.Fatal("different original binding adopted or mutated", got, err)
			}
		})
	}
}

func TestPendingHostedRecoveryRejectsDuplicateAndMalformedRetainedCandidates(t *testing.T) {
	for _, damage := range []string{"duplicate", "configuration", "attempt-revision"} {
		t.Run(damage, func(t *testing.T) {
			f, a, _ := hostedRecoveryFixture(t)
			a = pendingRecoveryAdvance(t, f, a, "uncertain", "interrupted", "")
			switch damage {
			case "duplicate":
				// Simulate missing reservation integrity only in disposable SQLite.
				// A second matching attempt must fail closed, never choose by ID.
				r := f.enqueue(t, attemptProject, "synthetic-duplicate", "hosted", 4, 1)
				second := "review_attempt_" + strings.Repeat("f", 32)
				admissionSQL(t, f, `INSERT INTO review_execution_attempts(id,operation_id,project_id,provider,account,state,revision,anchor_json,intent,external_id) SELECT ?,?,project_id,provider,account,state,revision,anchor_json,intent,external_id FROM review_execution_attempts WHERE id=?`, second, r.ID, a.ID)
				admissionSQL(t, f, `UPDATE review_operations SET state='uncertain',attempt_id=? WHERE id=?`, second, r.ID)
			case "configuration":
				admissionSQL(t, f, `UPDATE review_execution_config SET body_json=body_json||' ' WHERE project_id=? AND revision=1`, attemptProject)
			case "attempt-revision":
				admissionSQL(t, f, `UPDATE review_execution_attempts SET revision=1 WHERE id=?`, a.ID)
			}
			before := hostedObservationState(t, f, a)
			got, err := f.e.PendingHostedRecovery(t.Context(), "test-capability")
			attemptCode(t, err, "storage_unavailable")
			if got != nil || hostedObservationState(t, f, a) != before {
				t.Fatal("ambiguous/corrupt recovery selected or repaired", got)
			}
		})
	}
}

func TestPendingHostedRecoveryValidatesBeforeStorageAndHonorsCancellation(t *testing.T) {
	f := newAttemptFixture(t)
	got, err := f.e.PendingHostedRecovery(t.Context(), "test-capability")
	if err != nil || got != nil {
		t.Fatal("empty store not empty", got, err)
	}
	f, a, _ := hostedRecoveryFixture(t)
	a = pendingRecoveryAdvance(t, f, a, "uncertain", "interrupted", "")
	before := hostedObservationState(t, f, a)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err = f.e.PendingHostedRecovery(ctx, "test-capability")
	attemptCode(t, err, "overloaded")
	if got != nil || hostedObservationState(t, f, a) != before {
		t.Fatal("cancelled read selected or changed state")
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = f.e.PendingHostedRecovery(nil, "test-capability")
	attemptCode(t, err, "validation_failed")
	_, err = f.e.PendingHostedRecovery(t.Context(), "../invalid")
	attemptCode(t, err, "validation_failed")
	_, err = f.e.PendingHostedRecovery(t.Context(), "absent")
	attemptCode(t, err, "review_conflict")
	capability := f.e.capabilities["test-capability"]
	capability.Channels = []string{"cli"}
	f.e.capabilities["test-capability"] = capability
	_, err = f.e.PendingHostedRecovery(t.Context(), "test-capability")
	attemptCode(t, err, "review_conflict")
}

func TestPendingHostedRecoveryUsesOperationStateIndex(t *testing.T) {
	f, a, _ := hostedRecoveryFixture(t)
	a = pendingRecoveryAdvance(t, f, a, "uncertain", "interrupted", "")
	c := f.e.capabilities["test-capability"]
	rows, err := f.store.DB().Query("EXPLAIN QUERY PLAN "+pendingHostedRecoverySQL, c.Provider, c.Account, c.Fingerprint, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "review_operations_pending") {
			indexed = true
		}
		if strings.Contains(detail, "SCAN o") || strings.Contains(detail, "USE TEMP B-TREE") {
			t.Fatal("candidate selection lost indexed state bound", detail)
		}
	}
	if rows.Err() != nil || !indexed {
		t.Fatal("candidate selection must use existing state index", rows.Err())
	}
}
