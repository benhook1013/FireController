package reviewexec

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func statusHTTP(t *testing.T, f *attemptFixture, id, query, prefer string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/v1/projects/"+attemptProject+"/reviews/operations/"+id+"/status"+query, nil)
	r.SetPathValue("project_id", attemptProject)
	r.SetPathValue("operation_id", id)
	r.Header.Set("Authorization", "Bearer "+f.reader)
	if prefer != "" {
		r.Header.Set("Prefer", prefer)
	}
	w := httptest.NewRecorder()
	f.e.readOperationStatusHTTP(w, r)
	return w
}
func TestOperationStatusEvidenceIdentityAndCompactHTTP(t *testing.T) {
	f, a, in := evidenceFixture(t, "cli", true)
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	before := evidenceState(t, f, a)
	first, err := f.e.ReadOperationStatus(t.Context(), scope, a.OperationID)
	if err != nil || first.Stage != "provider_wait" || first.NextAction != "wait" || first.EvidenceID != "" {
		t.Fatal(first, err)
	}
	capture, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := f.e.ReadOperationStatus(t.Context(), scope, a.OperationID)
	if err != nil || retained.EvidenceID != capture.ID || retained.Stage != "result_captured" || retained.State != "active" || retained.NextAction != "wait" {
		t.Fatal("capture invented terminal operation", retained, err)
	}
	if _, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "controlled_completion", ""); err != nil {
		t.Fatal(err)
	}
	complete, err := f.e.ReadOperationStatus(t.Context(), scope, a.OperationID)
	if err != nil || complete.Stage != "evidence_ready" || complete.NextAction != "read_evidence" || complete.EvidenceID != capture.ID {
		t.Fatal(complete, err)
	}
	state := evidenceState(t, f, a)
	w := statusHTTP(t, f, a.OperationID, "", "wait=0")
	if w.Code != 200 || w.Header().Get("Preference-Applied") != "wait=0" || len(w.Body.Bytes()) > 1024 {
		t.Fatal(w.Code, w.Body.String())
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(w.Body.Bytes(), &fields) != nil || len(fields) != 13 {
		t.Fatal("unexpected compact fields", w.Body.String())
	}
	for _, secret := range []string{"project_id", "actor_id", "worker_id", "configuration_revision", "capability_fingerprint", "head_sha", "allowance", "Literal", "source text", "private-test-account"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("private/full diagnostics escaped", secret)
		}
	}
	if state != evidenceState(t, f, a) || before == state {
		t.Fatal("compact read changed journal or fixture never advanced")
	}
	for _, q := range []string{"?view=full", "?cursor=bad"} {
		if got := statusHTTP(t, f, a.OperationID, q, ""); got.Code != 400 {
			t.Fatal("unsupported selector accepted", got.Code)
		}
	}
	if got := statusHTTP(t, f, a.OperationID, "", "wait=901"); got.Code != 400 {
		t.Fatal("unbounded wait accepted", got.Code)
	}
	// An operation pointing at a foreign attempt must never borrow its evidence.
	second := f.start(t, attemptProject, f.enqueue(t, attemptProject, "another-status-op", "cli", 4, 1))
	if _, err = f.store.DB().Exec(`UPDATE review_operations SET attempt_id=? WHERE id=?`, second.ID, a.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.ReadOperationStatus(t.Context(), scope, a.OperationID); err == nil {
		t.Fatal("cross-operation attempt binding accepted")
	}
}
func TestOperationStatusMissingAndIncompleteEvidenceDoNotInventCoverage(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_capture", true: "incomplete_capture"}[capture], func(t *testing.T) {
			f, a, in := evidenceFixture(t, "cli", true)
			if capture {
				in.Review = reviewprovider.ParsedReview{Outcome: "failed", Complete: false, ReviewedFiles: []string{}, Findings: []reviewprovider.ParsedFinding{}}
				if _, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "controlled_completion", ""); err != nil {
				t.Fatal(err)
			}
			v, err := f.e.ReadOperationStatus(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), a.OperationID)
			if err != nil || v.NextAction != "contact_controller_owner" {
				t.Fatal(v, err)
			}
			if capture {
				if v.EvidenceID == "" || v.Stage != "capture_blocked" {
					t.Fatal(v)
				}
			} else if v.Stage != "unknown" || v.EvidenceID != "" {
				t.Fatal(v)
			}
		})
	}
}
func TestOperationStatusStageOnlyUsesDurableFacts(t *testing.T) {
	cases := []struct{ state, channel, reason, stage, next string }{{"queued", "hosted", "", "queued", "wait"}, {"preparing", "cli", "", "preparing", "wait"}, {"active", "hosted", "trigger_posted", "provider_wait", "wait"}, {"uncertain", "hosted", "hosted_finished_inventory_unproven", "capture_blocked", "contact_controller_owner"}, {"uncertain", "cli", "hosted_finished_inventory_unproven", "uncertain", "contact_controller_owner"}, {"held", "cli", "paused", "held", "read_diagnostics"}, {"failed", "cli", "failure", "failed", "read_diagnostics"}, {"cancelled", "hosted", "cancelled", "cancelled", "none"}}
	for _, c := range cases {
		stage, next := operationStatusStage(Operation{State: c.state, Channel: c.channel, Reason: c.reason}, "", false)
		if stage != c.stage || next != c.next {
			t.Fatal(c, stage, next)
		}
	}
}
func TestOperationStatusWaitCancellationAndFreshRevocation(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "revoke"}[revoke], func(t *testing.T) {
			f := sourceHTTPFixture(t)
			r := f.enqueue(t, attemptProject, "status-wait", "cli", 2, 1)
			scope := operationReadScope(t, f)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := f.e.WaitOperationStatus(ctx, scope, r.ID, 900); done <- err }()
			operationWaitRegistered(t, f.e, 1)
			if revoke {
				if _, err := f.store.DB().Exec(`UPDATE grants SET can_read=0 WHERE credential_id='exec-reader-cred'`); err != nil {
					t.Fatal(err)
				}
				f.e.notifyOperation(r.ID)
			} else {
				cancel()
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("wait did not end")
			}
			var failure *core.Error
			if !errors.As(err, &failure) {
				t.Fatal("missing safe wait failure", err)
			}
			want := "overloaded"
			if revoke {
				want = "forbidden"
			}
			if failure.Code != want {
				t.Fatal(failure.Code, want)
			}
			operationWaitRegistered(t, f.e, 0)
		})
	}
}

func TestOperationStatusHostedEvidenceAndProjectIsolation(t *testing.T) {
	f, a, in := standingEvidenceFixture(t)
	captured, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "capture_complete", a.ExternalID); err != nil {
		t.Fatal(err)
	}
	before := evidenceState(t, f, a)
	v, err := f.e.ReadOperationStatus(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), a.OperationID)
	if err != nil || v.Channel != "hosted" || v.Stage != "evidence_ready" || v.EvidenceID != captured.ID || v.NextAction != "read_evidence" {
		t.Fatal("real Hosted ledger binding failed", v, err)
	}
	_, err = f.e.ReadOperationStatus(t.Context(), evidenceReadScope(t, f, otherAttemptProject, f.other), a.OperationID)
	attemptCode(t, err, "not_found")
	if before != evidenceState(t, f, a) {
		t.Fatal("Hosted status changed evidence or accounting")
	}
}

func TestOperationStatusMissingCooldownDeadlineStaysUnknown(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "no-cooldown-observation", "hosted", 2, 1))
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, 1, "held", "hosted_allowance_cooldown", ""); err != nil {
		t.Fatal(err)
	}
	before := attemptState(t, f, a)
	w := statusHTTP(t, f, a.OperationID, "", "")
	var got OperationStatus
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.RetryNotBefore != "" || strings.Contains(w.Body.String(), "retry_not_before") || got.State != "held" {
		t.Fatal("missing deadline was invented or hold changed", w.Code, w.Body.String())
	}
	if attemptState(t, f, a) != before {
		t.Fatal("read changed retained state")
	}
}
