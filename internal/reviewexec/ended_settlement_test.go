package reviewexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func endedSettlementFixture(t *testing.T, channel string) (*attemptFixture, Attempt) {
	t.Helper()
	var f *attemptFixture
	var a Attempt
	var err error
	if channel == "hosted" {
		var input HostedObservationInput
		f, a, input = hostedObservationFixture(t)
		if _, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input); err != nil {
			t.Fatal(err)
		}
		a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "uncertain", "hosted_finished_inventory_unproven", "")
	} else {
		var evidence ReviewEvidence
		f, a, evidence = cliPublicationFixture(t, 4, "uncertain")
		adjudicationDecide(t, f, evidence.ID, "partial-accept", 0, 0, "accepted")
		if _, err = f.e.RecordFindingFix(t.Context(), f.scope(t, attemptProject, f.writer, false), "partial-fix", evidence.ID, FindingFixRequest{ExpectedRevision: adjudicationRevision(1), Finding: 0, ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("a", 40), ProofNote: "Independent retained fix proof"}); err != nil {
			t.Fatal(err)
		}
		adjudicationDecide(t, f, evidence.ID, "partial-reject", 2, 1, "rejected")
		if _, err = f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "partial-route-queue", Configuration{ExpectedRevision: adjudicationRevision(1), CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4, 6}, Channels: []string{"cli", "hosted"}}); err != nil {
			t.Fatal(err)
		}
		routeCreate(t, f, evidence.ID, "partial-route", routeRequest(3, 2, 4))
		if _, err = f.e.PublishEvidence(t.Context(), attemptProject, evidence.ID, cliPublicationPull(2)); err != nil {
			t.Fatal(err)
		}
		reason := "coverage_incomplete"
		if channel == "cli-interrupted" {
			reason = "execution_interrupted"
		}
		a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "uncertain", reason, "")
	}
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, f.store)
	f.handler = mux
	return f, a
}

func TestEndedSettlementRejectsMissingAndChangedOriginalAuthority(t *testing.T) {
	for _, channel := range []string{"hosted", "cli", "cli-interrupted"} {
		for _, mutation := range []struct{ name, sql string }{
			{"different-reason", `UPDATE review_operations SET reason='different_uncertainty' WHERE id=?;`},
			{"reservation-account", `UPDATE review_execution_reservations SET account='different_account' WHERE attempt_id=?`},
			{"reservation-class", `UPDATE review_execution_reservations SET action_class='different_class' WHERE attempt_id=?`},
			{"reservation-target", `UPDATE review_execution_reservations SET pull_number=6 WHERE attempt_id=?`},
			{"missing-reservation", `DELETE FROM review_execution_reservations WHERE attempt_id=?`},
			{"admission-state", `UPDATE review_execution_transitions SET state='preparing' WHERE attempt_id=? AND revision=2`},
			{"external-binding", `UPDATE review_execution_transitions SET external_id='different_external' WHERE attempt_id=? AND revision=3`},
		} {
			t.Run(channel+"/"+mutation.name, func(t *testing.T) {
				f, a := endedSettlementFixture(t, channel)
				id := a.ID
				if mutation.name == "different-reason" {
					id = a.OperationID
					admissionSQL(t, f, `UPDATE review_execution_transitions SET reason='different_uncertainty' WHERE attempt_id=? AND revision=?`, a.ID, a.Revision)
				}
				admissionSQL(t, f, mutation.sql, id)
				before := transferAuthorityRows(t, f, true)
				settlementError(t, hostedSettlementCall(t, f, a, "invalid-original", hostedSettlementBody(a)), 409, "review_settlement_unavailable")
				if !reflect.DeepEqual(before, transferAuthorityRows(t, f, true)) {
					t.Fatal("refusal changed original authority")
				}
			})
		}
	}
	for _, change := range []struct {
		name   string
		change func(*ReviewEvidence)
	}{
		{"indexed-evidence-id", func(r *ReviewEvidence) { r.ID = "review_evidence_" + strings.Repeat("e", 32) }},
		{"source-project", func(r *ReviewEvidence) { r.ProjectID = otherAttemptProject }},
		{"source-attempt", func(r *ReviewEvidence) { r.AttemptID = "review_attempt_" + strings.Repeat("e", 32) }},
		{"source-operation", func(r *ReviewEvidence) { r.OperationID = "review_operation_" + strings.Repeat("e", 32) }},
		{"capability", func(r *ReviewEvidence) { r.CapabilityFingerprint = strings.Repeat("e", 64) }},
		{"original-anchor", func(r *ReviewEvidence) { r.Anchor.BaseSHA = strings.Repeat("e", 40) }},
		{"capture-anchor", func(r *ReviewEvidence) { r.Capture.ExpectedAnchor.ParentSHA = strings.Repeat("e", 40) }},
		{"capture-id", func(r *ReviewEvidence) { r.Capture.CaptureID += "e" }},
		{"complete-review", func(r *ReviewEvidence) {
			r.Capture.Review.Complete, r.Capture.Review.Outcome, r.Capture.Review.ReportedFindings = true, "completed", 4
		}},
	} {
		t.Run("cli/"+change.name, func(t *testing.T) {
			f, a := endedSettlementFixture(t, "cli")
			var body string
			if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE attempt_id=?`, a.ID).Scan(&body); err != nil {
				t.Fatal(err)
			}
			var record ReviewEvidence
			if json.Unmarshal([]byte(body), &record) != nil {
				t.Fatal("invalid original fixture evidence")
			}
			change.change(&record)
			// Reseal the capture digest to prove semantic identity checks, rather
			// than relying solely on the fingerprint to detect forged records.
			digest := sha256.Sum256([]byte(encoded(record.Capture)))
			admissionSQL(t, f, `UPDATE review_execution_evidence SET body_json=?,fingerprint=? WHERE attempt_id=?`, encoded(record), hex.EncodeToString(digest[:]), a.ID)
			before := transferAuthorityRows(t, f, true)
			settlementError(t, hostedSettlementCall(t, f, a, "invalid-capture", hostedSettlementBody(a)), 409, "review_settlement_unavailable")
			if !reflect.DeepEqual(before, transferAuthorityRows(t, f, true)) {
				t.Fatal("invalid capture refusal changed source/controls")
			}
		})
	}
	for _, tc := range []struct{ name, channel, sql string }{
		{"missing-evidence", "cli", `DELETE FROM review_execution_evidence WHERE attempt_id=?`},
		{"fingerprint", "cli", `UPDATE review_execution_evidence SET fingerprint='invalid' WHERE attempt_id=?`},
		{"noncanonical-evidence", "cli", `UPDATE review_execution_evidence SET body_json=body_json||' ' WHERE attempt_id=?`},
		{"missing-observations", "hosted", `DELETE FROM review_hosted_observations WHERE attempt_id=?`},
		{"unbound-observation", "hosted", `UPDATE review_hosted_observations SET capture_sha256='invalid' WHERE attempt_id=?`},
	} {
		t.Run(tc.channel+"/"+tc.name, func(t *testing.T) {
			var f *attemptFixture
			var a Attempt
			if tc.name == "missing-evidence" {
				// An uncertain CLI attempt without any retained capture cannot use
				// the partial-result path, even with the selected reason code.
				f, a, _ = evidenceFixture(t, "cli", true)
				var err error
				a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "uncertain", "coverage_incomplete", "")
				if err != nil {
					t.Fatal(err)
				}
				mux := http.NewServeMux()
				f.e.Register(mux)
				f.handler = mux
			} else {
				f, a = endedSettlementFixture(t, tc.channel)
			}
			admissionSQL(t, f, tc.sql, a.ID)
			before := transferAuthorityRows(t, f, true)
			settlementError(t, hostedSettlementCall(t, f, a, "invalid-retention", hostedSettlementBody(a)), 409, "review_settlement_unavailable")
			if !reflect.DeepEqual(before, transferAuthorityRows(t, f, true)) {
				t.Fatal("retention refusal changed source")
			}
		})
	}
	for _, channel := range []string{"hosted", "cli", "cli-interrupted"} {
		t.Run(channel+"/active-is-not-ended", func(t *testing.T) {
			f, a := endedSettlementFixture(t, channel)
			var err error
			a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "retained_action_active", "")
			if err != nil {
				t.Fatal(err)
			}
			before := transferAuthorityRows(t, f, true)
			settlementError(t, hostedSettlementCall(t, f, a, "active-is-not-ended", hostedSettlementBody(a)), 409, "review_settlement_unavailable")
			if !reflect.DeepEqual(before, transferAuthorityRows(t, f, true)) {
				t.Fatal("live action was changed by settlement refusal")
			}
		})
	}
}

func TestEndedSettlementAdminGuardAuditFailureAndExactRetry(t *testing.T) {
	for _, channel := range []string{"hosted", "cli", "cli-interrupted"} {
		t.Run(channel, func(t *testing.T) {
			f, a := endedSettlementFixture(t, channel)
			before := transferAuthorityRows(t, f, true)
			settlementError(t, f.request("POST", hostedSettlementPath(attemptProject, a), f.writer, "project-write-denied", encoded(hostedSettlementBody(a))), 403, "forbidden")
			if !reflect.DeepEqual(before, transferAuthorityRows(t, f, true)) {
				t.Fatal("nonadmin refusal changed local work")
			}
			admissionSQL(t, f, `CREATE TRIGGER reject_ended_audit BEFORE INSERT ON events WHEN NEW.event_type IN ('review.hosted_settled','review.cli_settled') BEGIN SELECT RAISE(FAIL,'controlled audit failure'); END`)
			before = transferAuthorityRows(t, f, true)
			r := hostedSettlementCall(t, f, a, "atomic-ended-recovery", hostedSettlementBody(a))
			if r.Code != 500 || !reflect.DeepEqual(before, transferAuthorityRows(t, f, true)) {
				t.Fatal("audit failure did not roll back journal/projection/release/receipt", r.Code)
			}
			admissionSQL(t, f, `DROP TRIGGER reject_ended_audit`)
			r = hostedSettlementCall(t, f, a, "atomic-ended-recovery", hostedSettlementBody(a))
			if r.Code != 200 {
				t.Fatal("same key could not recover after rollback", r.Code)
			}
			settlementError(t, hostedSettlementCall(t, f, a, "new-key-stale-guard", hostedSettlementBody(a)), 409, "review_attempt_revision_conflict")
		})
	}
}

func TestEndedSettlementPublicRouteRetainsOriginalSource(t *testing.T) {
	for _, channel := range []string{"hosted", "cli", "cli-interrupted"} {
		t.Run(channel, func(t *testing.T) {
			f, a := endedSettlementFixture(t, channel)
			if channel == "cli-interrupted" {
				channel = "cli"
			}
			before := transferAuthorityRows(t, f, true)
			// CLI partial and unconfirmed Hosted finished inventory cannot borrow
			// explicit abandonment without the confirmed issued Hosted authority.
			settlementError(t, hostedSettlementCall(t, f, a, "unconfirmed-local-abandon", hostedAbandonmentBody(a)), 409, "review_settlement_unavailable")
			r := hostedSettlementCall(t, f, a, "ended-action", hostedSettlementBody(a))
			if r.Code != 200 {
				t.Fatalf("ended %s settlement: status=%d body=%s", channel, r.Code, r.Body.String())
			}
			wantState, wantReason, event := "cancelled", "operator_verified_external_stop", "review.hosted_settled"
			if channel == "cli" {
				wantState, wantReason, event = "failed", "cli_partial_settled", "review.cli_settled"
			}
			var receipt attemptSettlementReceipt
			if json.Unmarshal(r.Body.Bytes(), &receipt) != nil || receipt.AttemptID != a.ID || receipt.OperationID != a.OperationID || receipt.Revision != a.Revision+1 || receipt.State != wantState || receipt.Outcome != "settled" {
				t.Fatal("settlement receipt lost original identity/state")
			}
			after := transferAuthorityRows(t, f, true)
			for _, table := range []string{"review_execution_evidence", "review_hosted_observations", "review_adjudication_state", "review_adjudication_changes", "review_finding_routes", "review_finding_route_revisions", "review_route_policy_facts", "review_route_policy_invalidations", "review_run_versions", "review_finding_versions", "review_execution_config", "review_round_controls", "review_round_changes", "review_round_completions"} {
				original, existed := before[table]
				retained, exists := after[table]
				if channel == "cli" && !existed || existed != exists || !reflect.DeepEqual(original, retained) {
					t.Fatal("settlement changed original source/decisions/routes/credit", table)
				}
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE state='completed'`) != 0 {
				t.Fatal("settlement retained reservation or supplied completion")
			}
			var intent, state, reason, payload, actor string
			if err := f.store.DB().QueryRow(`SELECT a.intent,o.state,o.reason FROM review_execution_attempts a JOIN review_operations o ON o.id=a.operation_id WHERE a.id=?`, a.ID).Scan(&intent, &state, &reason); err != nil || intent != a.Intent || state != wantState || reason != wantReason {
				t.Fatal("wrong settled journal/intent", err)
			}
			if err := f.store.DB().QueryRow(`SELECT payload_json,actor_id FROM events WHERE event_type=? AND resource_id=?`, event, a.ID).Scan(&payload, &actor); err != nil {
				t.Fatal(err)
			}
			var audit attemptSettlementAudit
			if len(payload) > 4096 || json.Unmarshal([]byte(payload), &audit) != nil || audit.Anchor != *a.Anchor || audit.ExpectedRevision != a.Revision || !audit.ExternalActionStopped || audit.EvidenceReference != "operator-record:controlled-stop" || actor != f.scope(t, attemptProject, f.admin, true).PrincipalID {
				t.Fatal("settlement audit lost original binding or trusted attribution")
			}
			plan := SourcePlan{Repository: reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, Pull: reviewprovider.Pull{RepositoryID: 801, Number: 2, State: "open", HeadSHA: a.Anchor.CandidateSHA, BaseSHA: a.Anchor.ParentSHA, BaseRef: "main", ChangedFiles: 1}, ParentSHA: a.Anchor.ParentSHA, ObservedAt: stamp()}
			if covered, err := f.e.Coverage(t.Context(), attemptProject, plan, channel); err != nil || covered {
				t.Fatal("settlement granted coverage", covered, err)
			}
			if channel == "cli" {
				_, projected := projectedAttempt(t, f, Attempt{ID: a.ID})
				if projected.State != "failed" || projected.Reason != "cli_partial_settled" {
					t.Fatal("browser projection omitted failed partial settlement", projected)
				}
				var evidenceID string
				if err := f.store.DB().QueryRow(`SELECT id FROM review_execution_evidence WHERE attempt_id=?`, a.ID).Scan(&evidenceID); err != nil {
					t.Fatal(err)
				}
				detail := cliPublicationDetail(t, f, 2, evidenceID)
				if len(detail.Findings) != 4 || detail.Run.Complete || detail.Run.Attributable || detail.Run.Finalized {
					t.Fatal("partial source findings changed or gained credit")
				}
			}
			revision := int64(1)
			if channel == "cli" {
				revision = 2
			}
			next := f.enqueue(t, attemptProject, "fresh-explicit-request", channel, 2, revision)
			driver := &controlledDriver{dispatch: func(context.Context, Operation, Attempt, func(context.Context, string) error) (Result, error) {
				return Result{State: "failed", Reason: "synthetic_new_action_ended"}, nil
			}}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil || driver.calls.Load() != 1 {
				t.Fatal("fresh explicit action did not reach the controlled driver once", err, driver.calls.Load())
			}
			operation, err := f.e.ReadOperation(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), next.ID)
			if err != nil || operation.AttemptID == a.ID || operation.AttemptID == "" || operation.State != "failed" {
				t.Fatal("settlement reused old attempt or blocked fresh explicit work", err)
			}
			cliAllowanceReopen(t, f)
			mux := http.NewServeMux()
			f.e.Register(mux)
			f.handler = mux
			beforeReplay := transferAuthorityRows(t, f, true)
			replay := hostedSettlementCall(t, f, a, "ended-action", hostedSettlementBody(a))
			if replay.Code != 200 || replay.Body.String() != r.Body.String() || !reflect.DeepEqual(beforeReplay, transferAuthorityRows(t, f, true)) {
				t.Fatal("exact replay after restart changed later work")
			}
			changed := hostedSettlementBody(a)
			changed["evidence_reference"] = "different-operator-reference"
			settlementError(t, hostedSettlementCall(t, f, a, "ended-action", changed), 409, "idempotency_conflict")
		})
	}
}

// A process interruption is not a complete zero review. Only an explicit
// independently verified stop may release its original uncertain reservation.
func TestInterruptedCLISettlementZeroFindings(t *testing.T) {
	f, a, evidence := cliPublicationFixture(t, 0, "failed")
	if _, err := f.e.PublishEvidence(t.Context(), attemptProject, evidence.ID, cliPublicationPull(2)); err != nil {
		t.Fatal(err)
	}
	var err error
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "uncertain", "execution_interrupted", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		name    string
		token   string
		project string
		body    map[string]any
		status  int
		code    string
	}{
		{"false-stop", f.admin, attemptProject, hostedAbandonmentBody(a), 409, "review_settlement_unavailable"},
		{"writer", f.writer, attemptProject, hostedSettlementBody(a), 403, "forbidden"},
		{"foreign-project", f.admin, otherAttemptProject, hostedSettlementBody(a), 404, "not_found"},
	} {
		t.Run(check.name, func(t *testing.T) {
			before := transferAuthorityRows(t, f, true)
			r := f.request("POST", hostedSettlementPath(check.project, a), check.token, "interrupted-"+check.name, encoded(check.body))
			if r.Code != check.status {
				t.Fatalf("refusal status=%d want=%d", r.Code, check.status)
			}
			if !reflect.DeepEqual(before, transferAuthorityRows(t, f, true)) {
				t.Fatal("refusal mutated retained state")
			}
		})
	}
	before := transferAuthorityRows(t, f, true)
	r := hostedSettlementCall(t, f, a, "interrupted-zero", hostedSettlementBody(a))
	if r.Code != 200 {
		t.Fatalf("interrupted zero settlement: %d %s", r.Code, r.Body.String())
	}
	after := transferAuthorityRows(t, f, true)
	for _, table := range []string{"review_execution_evidence", "review_run_versions", "review_finding_versions", "review_execution_config", "review_round_controls", "review_round_changes", "review_round_completions"} {
		if _, ok := before[table]; !ok {
			t.Fatal("missing conservation table", table)
		}
		if !reflect.DeepEqual(before[table], after[table]) {
			t.Fatal("interrupted settlement changed original source/counts/controls", table)
		}
	}
	detail := cliPublicationDetail(t, f, 2, evidence.ID)
	if len(detail.Findings) != 0 || detail.Run.Complete || detail.Run.Attributable || detail.Run.Finalized || detail.Run.Found != 0 {
		t.Fatal("interruption gained findings/completion credit")
	}
	_, projected := projectedAttempt(t, f, Attempt{ID: a.ID})
	if projected.State != "failed" || projected.Reason != "cli_partial_settled" {
		t.Fatal("interrupted projection not failed")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 {
		t.Fatal("reservation not released")
	}
	settled := transferAuthorityRows(t, f, true)
	replay := hostedSettlementCall(t, f, a, "interrupted-zero", hostedSettlementBody(a))
	if replay.Code != 200 || replay.Body.String() != r.Body.String() || !reflect.DeepEqual(settled, transferAuthorityRows(t, f, true)) {
		t.Fatal("exact replay changed state")
	}
	settlementError(t, hostedSettlementCall(t, f, a, "interrupted-stale", hostedSettlementBody(a)), 409, "review_attempt_revision_conflict")
}

func TestInterruptedCLISettlementInvalidEvidence(t *testing.T) {
	for _, mutation := range []string{"missing", "hash", "complete"} {
		t.Run(mutation, func(t *testing.T) {
			var f *attemptFixture
			var a Attempt
			if mutation == "missing" {
				f, a, _ = evidenceFixture(t, "cli", true)
				var err error
				a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "uncertain", "execution_interrupted", "")
				if err != nil {
					t.Fatal(err)
				}
				mux := http.NewServeMux()
				f.e.Register(mux)
				f.handler = mux
			} else {
				f, a = endedSettlementFixture(t, "cli-interrupted")
			}
			switch mutation {
			case "missing":
				admissionSQL(t, f, `DELETE FROM review_execution_evidence WHERE attempt_id=?`, a.ID)
			case "hash":
				admissionSQL(t, f, `UPDATE review_execution_evidence SET fingerprint=? WHERE attempt_id=?`, strings.Repeat("e", 64), a.ID)
			case "complete":
				var body string
				if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE attempt_id=?`, a.ID).Scan(&body); err != nil {
					t.Fatal(err)
				}
				var record ReviewEvidence
				if err := json.Unmarshal([]byte(body), &record); err != nil {
					t.Fatal(err)
				}
				record.Capture.Review.Complete, record.Capture.Review.Outcome, record.Capture.Review.ReportedFindings = true, "completed", 4
				digest := sha256.Sum256([]byte(encoded(record.Capture)))
				admissionSQL(t, f, `UPDATE review_execution_evidence SET body_json=?,fingerprint=? WHERE attempt_id=?`, encoded(record), hex.EncodeToString(digest[:]), a.ID)
			}
			before := transferAuthorityRows(t, f, true)
			settlementError(t, hostedSettlementCall(t, f, a, "interrupted-invalid", hostedSettlementBody(a)), 409, "review_settlement_unavailable")
			if !reflect.DeepEqual(before, transferAuthorityRows(t, f, true)) {
				t.Fatal("invalid interrupted evidence changed state")
			}
		})
	}
}
