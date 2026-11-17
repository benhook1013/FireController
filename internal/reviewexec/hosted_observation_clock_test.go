package reviewexec

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestHostedObservationClockRollbackReadReplayAndRecovery(t *testing.T) {
	f, a, input := hostedObservationFixture(t)
	acceptedAt := time.Now().UTC().Add(time.Hour)
	input.StartedAt = acceptedAt.Add(-2 * time.Second).Format(time.RFC3339Nano)
	input.ObservedAt = acceptedAt.Add(-time.Second).Format(time.RFC3339Nano)
	input.FinishedAt = input.ObservedAt
	journal := attemptState(t, f, a)
	receipts := attemptCount(t, f.store, `SELECT count(*) FROM receipts`)
	receipt, err := f.e.recordHostedObservationAt(t.Context(), attemptProject, a.ID, input, acceptedAt)
	if err != nil {
		t.Fatal("observation valid at original recording clock rejected", err)
	}
	if attemptState(t, f, a) != journal || attemptCount(t, f.store, `SELECT count(*) FROM receipts`) != receipts || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
		t.Fatal("raw observation changed attempt, receipt or final evidence ledger")
	}
	before := hostedObservationState(t, f, a)
	records, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
	if err != nil || len(records) != 1 || records[0].Input != input || records[0].ID != receipt.ID {
		t.Fatal("rollback invalidated original recovery read", records, err)
	}
	replay, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
	if err != nil || replay != receipt || hostedObservationState(t, f, a) != before {
		t.Fatal("rollback invalidated exact replay or changed ledger", err)
	}
	changed := input
	changed.CaptureBytes++
	_, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, changed)
	attemptCode(t, err, "review_conflict")
	if hostedObservationState(t, f, a) != before {
		t.Fatal("changed replay mutated retained observation")
	}

	// Startup recovery must keep the original reference, without another post or
	// turning a raw observation into final evidence or settled review credit.
	cliAllowanceReopen(t, f)
	f.e, err = New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	before = hostedObservationState(t, f, a)
	reopened, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
	if err != nil || !reflect.DeepEqual(reopened, records) {
		t.Fatal("restart/clock rollback changed retained recovery references", err)
	}
	bound, err := f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "test-capability")
	if err != nil || bound.Attempt.State != "uncertain" || bound.EvidenceID != "" {
		t.Fatal("raw observation fabricated final recovery evidence", bound, err)
	}
	replay, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
	if err != nil || replay != receipt || hostedObservationState(t, f, a) != before {
		t.Fatal("interrupted replay failed or changed state", err)
	}
	if _, err = f.e.Advance(t.Context(), attemptProject, a.ID, bound.Attempt.Revision, "completed", "controlled_settlement", ""); err != nil {
		t.Fatal(err)
	}
	before = hostedObservationState(t, f, a)
	replay, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
	if err != nil || replay != receipt || hostedObservationState(t, f, a) != before {
		t.Fatal("settled replay after rollback failed", err)
	}

	// A canonical but structurally corrupt retained observation must still fail
	// closed. Clock independence does not relax ordering or indexed bindings.
	corrupt := records[0]
	corrupt.Input.ObservedAt = acceptedAt.Add(-3 * time.Second).Format(time.RFC3339Nano)
	body, err := json.Marshal(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `UPDATE review_hosted_observations SET body_json=? WHERE id=?`, string(body), receipt.ID)
	before = hostedObservationState(t, f, a)
	got, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
	attemptCode(t, err, "storage_unavailable")
	if got != nil {
		t.Fatal("corrupt observation escaped read")
	}
	_, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
	attemptCode(t, err, "storage_unavailable")
	if hostedObservationState(t, f, a) != before {
		t.Fatal("corrupt read/replay changed ledger")
	}
}

func TestHostedObservationClockFreshFutureAdmissionAndBoundary(t *testing.T) {
	for _, test := range []struct {
		name   string
		offset time.Duration
		public bool
		valid  bool
	}{
		{"public future", time.Hour, true, false},
		{"exact five-second bound", 5 * time.Second, false, true},
		{"beyond bound", 5*time.Second + time.Nanosecond, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, a, input := hostedObservationFixture(t)
			now := time.Now().UTC()
			input.FinishedAt = now.Add(test.offset).Format(time.RFC3339Nano)
			before := hostedObservationState(t, f, a)
			var err error
			if test.public {
				_, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
			} else {
				_, err = f.e.recordHostedObservationAt(t.Context(), attemptProject, a.ID, input, now)
			}
			if test.valid {
				if err != nil {
					t.Fatal("valid admission boundary rejected", err)
				}
			} else {
				attemptCode(t, err, "validation_failed")
				if hostedObservationState(t, f, a) != before {
					t.Fatal("future capture changed durable state")
				}
			}
		})
	}
}
