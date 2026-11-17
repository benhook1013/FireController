package reviewexec

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

// The explicit admission instant models a capture accepted before the host clock
// moved backwards. All subsequent operations use the actual, earlier clock.
func TestEvidenceClockRollbackRetainedLifecycle(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			var f *attemptFixture
			var a Attempt
			var input ReviewEvidenceInput
			if channel == "hosted" {
				f, a, input = hostedPublicationFixture(t, false)
			} else {
				f, a, input = evidenceFixture(t, "cli", false)
				input.ExpectedAnchor.ParentSHA = input.ExpectedAnchor.BaseSHA
				var err error
				a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, input.ExpectedAnchor, "")
				if err != nil {
					t.Fatal(err)
				}
				if err := EnsureAdjudicationSchema(t.Context(), f.store); err != nil {
					t.Fatal(err)
				}
				if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
					t.Fatal(err)
				}
				f.e, err = New(f.store, attemptCapabilities, WithNativeProjection())
				if err != nil {
					t.Fatal(err)
				}
			}
			acceptedAt := time.Now().UTC().Add(time.Hour)
			input.StartedAt = acceptedAt.Add(-2 * time.Second).Format(time.RFC3339Nano)
			input.FinishedAt = acceptedAt.Add(-time.Second).Format(time.RFC3339Nano)
			original := evidenceClone(input)
			r, err := f.e.recordReviewEvidenceAt(t.Context(), attemptProject, a.ID, input, acceptedAt)
			if err != nil {
				t.Fatal("capture valid at original admission instant rejected", err)
			}
			before := evidenceState(t, f, a)
			got, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), r.ID)
			if err != nil || !reflect.DeepEqual(got.Capture, original) {
				t.Fatal("rollback invalidated retained read", err)
			}
			if _, err := f.e.ReadReviewAdjudication(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), r.ID); err != nil {
				t.Fatal("rollback invalidated adjudication read", err)
			}
			replay, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, original)
			if err != nil || replay != r || evidenceState(t, f, a) != before {
				t.Fatal("rollback invalidated exact replay or changed state", replay, err)
			}
			changed := evidenceClone(input)
			changed.Review.Findings[0].Title = "Different retained input"
			_, err = f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, changed)
			attemptCode(t, err, "review_conflict")
			if evidenceState(t, f, a) != before {
				t.Fatal("changed replay mutated retained evidence")
			}

			// Initial publication and subsequent controls must use retained structural
			// facts, rather than reapplying a new-ingestion wall-clock check.
			pull := cliPublicationPull(2)
			if _, err := f.e.PublishEvidence(t.Context(), attemptProject, r.ID, pull); err != nil {
				t.Fatal("rollback invalidated publication", err)
			}
			adjudicationComplete(t, f, a)
			anchor := *a.Anchor
			current := SourcePlan{Repository: reviewprovider.Repository{ID: anchor.RepositoryID, FullName: anchor.Repository, DefaultBranch: "main"}, Pull: reviewprovider.Pull{Number: anchor.PullNumber, RepositoryID: anchor.RepositoryID, State: "open", HeadSHA: anchor.CandidateSHA, BaseSHA: anchor.BaseSHA, BaseRef: anchor.BaseRef, ChangedFiles: 1}, ParentSHA: anchor.ParentSHA, ObservedAt: stamp()}
			comparison, err := f.e.Comparison(t.Context(), attemptProject, current, channel)
			if err != nil || comparison != (ComparisonState{Pending: true}) {
				t.Fatal("rollback hid pending retained evidence", comparison, err)
			}
			for i := range input.Review.Findings {
				adjudicationDecide(t, f, r.ID, "clock-decision-"+strings.Repeat("x", i+1), int64(i), int64(i), "accepted")
			}
			revision := int64(len(input.Review.Findings))
			body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "clock-finalize", r.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(revision)})
			adjudicationReceipt(t, body, err, r.ID, "finalized", revision+1)
			revision++
			for i := range input.Review.Findings {
				coverageFix(t, f, r.ID, "clock-fix-"+strings.Repeat("x", i+1), revision, int64(i), "")
				revision++
			}
			covered, err := f.e.Coverage(t.Context(), attemptProject, current, channel)
			if err != nil || !covered {
				t.Fatal("rollback invalidated finalized fixed coverage", covered, err)
			}
			if _, err := f.e.PublishEvidence(t.Context(), attemptProject, r.ID, nil); err != nil {
				t.Fatal("rollback invalidated publication replay", err)
			}
			retained, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), r.ID)
			if err != nil || !reflect.DeepEqual(retained, got) {
				t.Fatal("controls changed immutable source after rollback", err)
			}
			before = evidenceState(t, f, a)
			replay, err = f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, original)
			if err != nil || replay != r || evidenceState(t, f, a) != before {
				t.Fatal("settled replay changed after rollback", err)
			}
			if channel == "hosted" {
				recovery, err := f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "test-capability")
				if err != nil || recovery.EvidenceID != r.ID {
					t.Fatal("rollback invalidated retained recovery evidence", recovery, err)
				}
			}
		})
	}
}

func TestEvidenceClockFreshIngestionStillRejectsFutureAndMalformed(t *testing.T) {
	for _, kind := range []string{"future", "unordered", "utf8"} {
		t.Run(kind, func(t *testing.T) {
			f, a, input := evidenceFixture(t, "cli", true)
			switch kind {
			case "future":
				input.FinishedAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
			case "unordered":
				input.StartedAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
			case "utf8":
				input.Review.Findings[0].Detail = string([]byte{0xff})
			}
			before := evidenceState(t, f, a)
			_, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, input)
			attemptCode(t, err, "validation_failed")
			if evidenceState(t, f, a) != before {
				t.Fatal("invalid fresh capture changed durable state")
			}
		})
	}
}

func TestHostedRecoveryClockRollbackRetainedAdmission(t *testing.T) {
	f, a, _ := hostedRecoveryFixture(t)
	admitted := time.Now().UTC().Add(time.Hour)
	admissionSQL(t, f, `UPDATE review_operations SET created_at=? WHERE id=?`, admitted.Add(-time.Second).Format(time.RFC3339Nano), a.OperationID)
	admissionSQL(t, f, `UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=? AND revision=2`, admitted.Format(time.RFC3339Nano), a.ID)
	before := hostedObservationState(t, f, a)
	got, err := f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "test-capability")
	if err != nil || got.EarliestPosting != admitted.Truncate(time.Second) || hostedObservationState(t, f, a) != before {
		t.Fatal("clock rollback rejected or changed retained posting boundary", got, err)
	}
}
