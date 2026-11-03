package reviewexec

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviews"
)

func uncertainCapturedCLI(t *testing.T, count int, outcome string) (*attemptFixture, Attempt, ReviewEvidence) {
	t.Helper()
	f, a, evidence := cliPublicationFixture(t, count, outcome)
	var err error
	a, err = f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "uncertain", "capture_retention_failed", "")
	if err != nil {
		t.Fatal(err)
	}
	return f, a, evidence
}

func TestRecoverRetainedCLICompletesOriginalWithoutProviderOrFinalization(t *testing.T) {
	for _, count := range []int{0, 3} {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			f, a, evidence := uncertainCapturedCLI(t, count, "completed")
			pull := projectionPull(2)
			pull.HeadSHA, pull.Title = strings.Repeat("9", 40), "Current PR facts after capture"
			zero := int64(0)
			w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "recovery-metadata", encoded(reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: evidence.Anchor.RepositoryID, Source: "synthetic-import", ObservedAt: stamp(), Queue: []reviews.PullInput{pull}}))
			if w.Code != 201 {
				t.Fatal(w.Code, w.Body.String())
			}
			// Recovery observes the original admission, independent of later caller
			// revocation and project pause. Neither authorizes another execution.
			f.configure(t, attemptProject, "test-capability", 1, true)
			if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`); err != nil {
				t.Fatal(err)
			}
			trap := &cliPublicationHTTPTrap{}
			prior := http.DefaultTransport
			http.DefaultTransport = trap
			defer func() { http.DefaultTransport = prior }()
			if err := f.e.RecoverRetainedCLI(t.Context(), "test-capability"); err != nil {
				t.Fatal(err)
			}
			operation, err := f.e.ReadOperation(t.Context(), evidenceReadScope(t, f, a.ProjectID, f.reader), a.OperationID)
			if err != nil || operation.State != "completed" || operation.Reason != "cli_evidence_recovered" || operation.AttemptID != a.ID {
				t.Fatal(operation, err)
			}
			got, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, a.ProjectID, f.reader), evidence.ID)
			if err != nil || !reflect.DeepEqual(got, evidence) {
				t.Fatal("recovery changed original immutable evidence", err)
			}
			detail := cliPublicationDetail(t, f, 2, evidence.ID)
			if detail.Run.Found != int64(count) || detail.Run.HeadSHA != a.Anchor.CandidateSHA || detail.Run.Finalized || len(detail.Findings) != count || detail.Run.Accepted != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 {
				t.Fatal("recovery invented adjudication or lost original result", detail)
			}
			var current reviews.PullResponse
			projectionRead(t, f, "/pulls/2", &current)
			if current.Pull.HeadSHA != pull.HeadSHA || current.Pull.Title != pull.Title {
				t.Fatal("recovery overwrote current PR facts")
			}
			before := evidenceState(t, f, a) + projectionDigest(t, f, "")
			if err := f.e.RecoverRetainedCLI(t.Context(), "test-capability"); err != nil || before != evidenceState(t, f, a)+projectionDigest(t, f, "") || trap.calls.Load() != 0 {
				t.Fatal("replay repeated credit, publication or provider work", err)
			}
		})
	}
}

func TestRecoverRetainedCLILeavesPartialMissingAndLiveWorkAlone(t *testing.T) {
	for _, mode := range []string{"partial", "missing", "active", "changed-capability", "missing-reservation", "bad-evidence"} {
		t.Run(mode, func(t *testing.T) {
			outcome := "completed"
			if mode == "partial" {
				outcome = "uncertain"
			}
			f, a, evidence := uncertainCapturedCLI(t, 1, outcome)
			switch mode {
			case "missing":
				if _, err := f.store.DB().Exec(`DELETE FROM review_adjudication_state WHERE evidence_id=?`, evidence.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.store.DB().Exec(`DELETE FROM review_execution_evidence WHERE id=?`, evidence.ID); err != nil {
					t.Fatal(err)
				}
			case "active":
				var err error
				a, err = f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "active", "fixture_owned_active", "")
				if err != nil {
					t.Fatal(err)
				}
			case "changed-capability":
				caps := append([]Capability(nil), attemptCapabilities...)
				caps[0].Fingerprint = strings.Repeat("9", 64)
				var err error
				f.e, err = New(f.store, caps, WithNativeProjection())
				if err != nil {
					t.Fatal(err)
				}
			case "missing-reservation":
				if _, err := f.store.DB().Exec(`DELETE FROM review_execution_reservations WHERE attempt_id=?`, a.ID); err != nil {
					t.Fatal(err)
				}
			case "bad-evidence":
				if _, err := f.store.DB().Exec(`UPDATE review_execution_evidence SET fingerprint=? WHERE id=?`, strings.Repeat("0", 64), evidence.ID); err != nil {
					t.Fatal(err)
				}
			}
			before := evidenceState(t, f, a)
			err := f.e.RecoverRetainedCLI(context.Background(), "test-capability")
			wantError := mode == "missing-reservation" || mode == "bad-evidence"
			if (err != nil) != wantError || before != evidenceState(t, f, a) {
				t.Fatal("unproven recovery changed retained facts", err)
			}
		})
	}
}

func TestRecoverRetainedCLIMissingProjectionStillSettlesExactEvidence(t *testing.T) {
	f, a, evidence := uncertainCapturedCLI(t, 1, "completed")
	if err := f.e.RecoverRetainedCLI(t.Context(), "test-capability"); err != nil {
		t.Fatal(err)
	}
	operation, err := f.e.ReadOperation(t.Context(), evidenceReadScope(t, f, a.ProjectID, f.reader), a.OperationID)
	if err != nil || operation.State != "completed" || attemptCount(t, f.store, `SELECT count(*) FROM review_runs WHERE source_run_id=?`, evidence.ID) != 0 {
		t.Fatal("missing display metadata erased execution or invented a row", operation, err)
	}
}

func TestRecoverRetainedCLIPublicationFailureRetainsReservationForExactRetry(t *testing.T) {
	f, a, evidence := uncertainCapturedCLI(t, 1, "completed")
	// Seed current display facts without publishing the result itself.
	pull := projectionPull(2)
	zero := int64(0)
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "retry-metadata", encoded(reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: evidence.Anchor.RepositoryID, Source: "synthetic-import", ObservedAt: stamp(), Queue: []reviews.PullInput{pull}}))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := f.store.DB().Exec(`CREATE TRIGGER reject_recovery_publication BEFORE INSERT ON review_run_versions BEGIN SELECT RAISE(ABORT,'synthetic publication failure'); END`); err != nil {
		t.Fatal(err)
	}
	before := evidenceState(t, f, a) + projectionDigest(t, f, "")
	if err := f.e.RecoverRetainedCLI(t.Context(), "test-capability"); err == nil || before != evidenceState(t, f, a)+projectionDigest(t, f, "") {
		t.Fatal("failed publication changed original execution or released reservation", err)
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER reject_recovery_publication`); err != nil {
		t.Fatal(err)
	}
	if err := f.e.RecoverRetainedCLI(t.Context(), "test-capability"); err != nil {
		t.Fatal(err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions WHERE attempt_id=? AND state='completed'`, a.ID) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 {
		t.Fatal("exact recovery did not settle the original attempt once")
	}
}
