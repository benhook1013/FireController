package reviewexec

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func incompleteHostedFixture(t *testing.T) (*attemptFixture, Attempt, HostedObservationInput, time.Time) {
	t.Helper()
	f, a, in := hostedObservationFixture(t)
	if _, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, in); err != nil {
		t.Fatal(err)
	}
	var err error
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "trigger_recorded", "101")
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "uncertain", "hosted_finished_inventory_unproven", "")
	if err != nil {
		t.Fatal(err)
	}
	var at string
	if err := f.store.DB().QueryRow(`SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision).Scan(&at); err != nil {
		t.Fatal(err)
	}
	first, _ := time.Parse(time.RFC3339Nano, at)
	return f, a, in, first
}
func incompleteHostedSource(in HostedObservationInput) HostedIncompleteCapture {
	return HostedIncompleteCapture{in.CaptureID, in.CaptureSHA256, in.CaptureBytes}
}
func TestFinishedHostedReleaseGraceAndExactReservation(t *testing.T) {
	f, a, in, first := incompleteHostedFixture(t)
	// Another repository on the same provider account is independent.
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	other := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "other-hosted", "hosted", 2, 1))
	next := f.enqueue(t, attemptProject, "next-hosted", "hosted", 4, 1)
	blocked, err := f.e.StartAttempt(t.Context(), attemptProject, next.ID)
	if err == nil || blocked.ID != "" {
		t.Fatal("repository lane was not held", blocked, err)
	}
	before := hostedObservationState(t, f, a)
	for _, now := range []time.Time{first.Add(-time.Second), first, first.Add(HostedIncompleteGrace - time.Nanosecond)} {
		f.e.attemptNow = func() time.Time { return now }
		_, err = f.e.ReleaseFinishedHosted(t.Context(), attemptProject, a.ID, a.Revision, incompleteHostedSource(in))
		attemptCode(t, err, "hosted_incomplete_grace")
		if hostedObservationState(t, f, a) != before {
			t.Fatal("grace refusal mutated state")
		}
	}
	f.e.attemptNow = func() time.Time { return first.Add(HostedIncompleteGrace) }
	released, err := f.e.ReleaseFinishedHosted(t.Context(), attemptProject, a.ID, a.Revision, incompleteHostedSource(in))
	if err != nil || released.State != "failed" || released.ExternalID != "101" || released.Intent != a.Intent || released.Anchor == nil || *released.Anchor != *a.Anchor || released.Revision != a.Revision+1 {
		t.Fatal(released, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_hosted_observations`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, other.ID) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.hosted_incomplete_released'`) != 1 {
		t.Fatal("release changed evidence or another reservation")
	}
	if _, err := f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "test-capability"); err != nil {
		t.Fatal("original recovery identity unavailable", err)
	}
	started := f.start(t, attemptProject, next)
	if started.Anchor != nil || started.Intent != "" || started.ExternalID != "" {
		t.Fatal("release authorized an external post", started)
	}
	after := hostedObservationState(t, f, a)
	_, err = f.e.ReleaseFinishedHosted(t.Context(), attemptProject, a.ID, a.Revision, incompleteHostedSource(in))
	attemptCode(t, err, "review_conflict")
	if after != hostedObservationState(t, f, a) {
		t.Fatal("stale retry mutated state")
	}
	// Restart must not recreate the reservation or rediscover the failed request.
	cliAllowanceReopen(t, f)
	f.e, err = New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := f.e.PendingHostedRecoveries(t.Context(), "test-capability")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p.AttemptID == a.ID {
			t.Fatal("incomplete request resumed after restart")
		}
	}
}
func TestFinishedHostedReleaseRefusesInvalidProofAtomically(t *testing.T) {
	cases := map[string]func(*testing.T, *attemptFixture, Attempt, *HostedIncompleteCapture){
		"wrong checksum": func(t *testing.T, f *attemptFixture, a Attempt, s *HostedIncompleteCapture) {
			s.SHA256 = strings.Repeat("2", 64)
			s.CaptureID = "review_capture_" + s.SHA256[:32]
		},
		"wrong bytes": func(t *testing.T, f *attemptFixture, a Attempt, s *HostedIncompleteCapture) { s.Bytes++ },
		"missing observation": func(t *testing.T, f *attemptFixture, a Attempt, s *HostedIncompleteCapture) {
			admissionSQL(t, f, `DELETE FROM review_hosted_observations WHERE attempt_id=?`, a.ID)
		},
		"changed original trigger": func(t *testing.T, f *attemptFixture, a Attempt, s *HostedIncompleteCapture) {
			admissionSQL(t, f, `UPDATE review_execution_transitions SET external_id='102' WHERE attempt_id=? AND state='active'`, a.ID)
		},
		"unconfirmed post": func(t *testing.T, f *attemptFixture, a Attempt, s *HostedIncompleteCapture) {
			admissionSQL(t, f, `UPDATE review_execution_transitions SET reason='unknown_post' WHERE attempt_id=? AND state='active'`, a.ID)
		},
		"wrong reason": func(t *testing.T, f *attemptFixture, a Attempt, s *HostedIncompleteCapture) {
			admissionSQL(t, f, `UPDATE review_operations SET reason='hosted_observation_pending' WHERE id=?`, a.OperationID)
		},
		"missing reservation": func(t *testing.T, f *attemptFixture, a Attempt, s *HostedIncompleteCapture) {
			admissionSQL(t, f, `DELETE FROM review_execution_reservations WHERE attempt_id=?`, a.ID)
		},
		"changed reservation": func(t *testing.T, f *attemptFixture, a Attempt, s *HostedIncompleteCapture) {
			admissionSQL(t, f, `UPDATE review_execution_reservations SET account='other-account' WHERE attempt_id=?`, a.ID)
		},
		"changed journal": func(t *testing.T, f *attemptFixture, a Attempt, s *HostedIncompleteCapture) {
			admissionSQL(t, f, `UPDATE review_hosted_observations SET body_json='{}' WHERE attempt_id=?`, a.ID)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f, a, in, first := incompleteHostedFixture(t)
			f.e.attemptNow = func() time.Time { return first.Add(HostedIncompleteGrace) }
			s := incompleteHostedSource(in)
			change(t, f, a, &s)
			before := hostedObservationState(t, f, a)
			if _, err := f.e.ReleaseFinishedHosted(t.Context(), attemptProject, a.ID, a.Revision, s); err == nil {
				t.Fatal("accepted invalid stop proof")
			}
			if before != hostedObservationState(t, f, a) {
				t.Fatal("invalid proof changed state")
			}
		})
	}
}
func TestFinishedHostedReleaseLatestSourceAndEvidenceRace(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(map[bool]string{false: "newer observation", true: "final evidence"}[complete], func(t *testing.T) {
			f, a, in, first := incompleteHostedFixture(t)
			f.e.attemptNow = func() time.Time { return first.Add(HostedIncompleteGrace) }
			if complete {
				evidence := hostedSettlementFinalInput(a, in)
				if _, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, evidence); err != nil {
					t.Fatal(err)
				}
			} else {
				newer := in
				newer.CaptureSHA256 = strings.Repeat("2", 64)
				newer.CaptureID = "review_capture_" + newer.CaptureSHA256[:32]
				if _, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, newer); err != nil {
					t.Fatal(err)
				}
			}
			before := hostedObservationState(t, f, a)
			if _, err := f.e.ReleaseFinishedHosted(t.Context(), attemptProject, a.ID, a.Revision, incompleteHostedSource(in)); err == nil {
				t.Fatal("released a superseded source")
			}
			if before != hostedObservationState(t, f, a) {
				t.Fatal("race refusal mutated state")
			}
		})
	}
}
func TestFinishedHostedReleaseAuditRollbackAndGuardedEntry(t *testing.T) {
	f, a, in, first := incompleteHostedFixture(t)
	f.e.attemptNow = func() time.Time { return first.Add(HostedIncompleteGrace) }
	before := hostedObservationState(t, f, a)
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "failed", HostedFinishedIncompleteReason, ""); err == nil {
		t.Fatal("unguarded incomplete release allowed")
	}
	if before != hostedObservationState(t, f, a) {
		t.Fatal("unguarded entry mutated state")
	}
	admissionSQL(t, f, `CREATE TRIGGER block_incomplete_event BEFORE INSERT ON events WHEN NEW.event_type='review.hosted_incomplete_released' BEGIN SELECT RAISE(FAIL,'controlled audit failure'); END`)
	if _, err := f.e.ReleaseFinishedHosted(context.Background(), attemptProject, a.ID, a.Revision, incompleteHostedSource(in)); err == nil {
		t.Fatal("expected audit failure")
	}
	if before != hostedObservationState(t, f, a) {
		t.Fatal("audit failure partially released reservation")
	}
	admissionSQL(t, f, `DROP TRIGGER block_incomplete_event`)
	if _, err := f.e.ReleaseFinishedHosted(t.Context(), attemptProject, a.ID, a.Revision, incompleteHostedSource(in)); err != nil {
		t.Fatal(err)
	}
}

func TestFinishedHostedReleaseConcurrentEvidence(t *testing.T) {
	for i := 0; i < 6; i++ {
		f, a, in, first := incompleteHostedFixture(t)
		f.e.attemptNow = func() time.Time { return first.Add(HostedIncompleteGrace) }
		start := make(chan struct{})
		released := make(chan error, 1)
		captured := make(chan error, 1)
		go func() {
			<-start
			_, err := f.e.ReleaseFinishedHosted(context.Background(), attemptProject, a.ID, a.Revision, incompleteHostedSource(in))
			released <- err
		}()
		go func() {
			<-start
			_, err := f.e.RecordReviewEvidence(context.Background(), attemptProject, a.ID, hostedSettlementFinalInput(a, in))
			captured <- err
		}()
		close(start)
		releaseErr, captureErr := <-released, <-captured
		if (releaseErr == nil) == (captureErr == nil) {
			t.Fatal("race did not have exactly one winner", releaseErr, captureErr)
		}
		wantEvidence, wantReservation, wantAudit := 0, 0, 1
		if captureErr == nil {
			wantEvidence, wantReservation, wantAudit = 1, 1, 0
		}
		if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, a.ID) != wantEvidence || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != wantReservation || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.hosted_incomplete_released' AND resource_id=?`, a.ID) != wantAudit {
			t.Fatal("release and completion were not atomic")
		}
	}
}

func TestFinishedHostedReleaseAutomaticallySelectsNextWithoutRetry(t *testing.T) {
	f := schedulingFixture(t)
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	original := schedulingLatest(t, f)
	a := f.start(t, attemptProject, Receipt{ID: original.OperationID})
	anchor := attemptAnchor(attemptProject, 2)
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: anchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, intent)
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "trigger_recorded", "101")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("1", 64)
	observed := time.Now().UTC().Format(time.RFC3339Nano)
	in := HostedObservationInput{CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1024, StartedAt: observed, FinishedAt: observed, ObservedAt: observed, ExpectedAnchor: anchor, Intent: intent}
	if _, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, in); err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "uncertain", "hosted_finished_inventory_unproven", "")
	if err != nil {
		t.Fatal(err)
	}
	schedulingGenerate(t, f, false)
	f.e.attemptNow = func() time.Time { return time.Now().Add(HostedIncompleteGrace + time.Second) }
	if _, err := f.e.ReleaseFinishedHosted(t.Context(), attemptProject, a.ID, a.Revision, incompleteHostedSource(in)); err != nil {
		t.Fatal(err)
	}
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	next := schedulingLatest(t, f)
	op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), next.OperationID)
	if err != nil || next.State != "selected" || op.PullNumber != 4 || next.OperationID == original.OperationID {
		t.Fatal("release did not select distinct eligible successor", next, op, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE project_id=? AND pull_number=2 AND channel='hosted'`, attemptProject) != 1 {
		t.Fatal("incomplete review created its own replacement")
	}
	schedulingGenerate(t, f, false)
	if roundRead(t, f, 2, "hosted").Used != 0 || roundRead(t, f, 4, "hosted").Used != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
		t.Fatal("lane progression manufactured credit")
	}
}
