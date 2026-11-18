package reviewexec

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRetainedCLIReplayRejectsNoncanonicalBody(t *testing.T) {
	f, a, in, _ := cliWaitFixture(t)
	if err := f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, "UPDATE review_cli_preflight SET body_json=body_json||' ' WHERE attempt_id=?", a.ID)
	if err := f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, 1, in); err == nil {
		t.Fatal("noncanonical replay accepted")
	}
}

func TestRetainedCooldownReplayRequiresCaptureReference(t *testing.T) {
	f := supervisorFixture(t)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "replay-reference", "hosted", 2, 1))
	in := retainTimerCooldown(t, f, a, 6*time.Minute)
	if err := f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, "DELETE FROM review_hosted_observations WHERE attempt_id=?", a.ID)
	if err := f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, in); err == nil {
		t.Fatal("missing capture accepted")
	}
}

func TestRetainedHostedJournalRejectsInvalidStorage(t *testing.T) {
	for _, damage := range []string{"blob", "oversized", "position", "bytes"} {
		t.Run(damage, func(t *testing.T) {
			f, a, in := hostedObservationFixture(t)
			if _, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, in); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "blob":
				admissionSQL(t, f, "UPDATE review_hosted_observations SET body_json=CAST(body_json AS BLOB) WHERE attempt_id=?", a.ID)
			case "oversized":
				admissionSQL(t, f, "UPDATE review_hosted_observations SET body_json=? WHERE attempt_id=?", strings.Repeat("x", hostedObservationInputLimit+2049), a.ID)
			case "position":
				admissionSQL(t, f, "UPDATE review_hosted_observations SET position=1.5 WHERE attempt_id=?", a.ID)
			case "bytes":
				admissionSQL(t, f, "UPDATE review_hosted_observations SET capture_bytes=1.5 WHERE attempt_id=?", a.ID)
			}
			before := hostedObservationState(t, f, a)
			_, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
			if !isStoredUnavailable(err, storedHostedObservation) {
				t.Fatal("read wrong classification", err)
			}
			_, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, in)
			if !isStoredUnavailable(err, storedHostedObservation) {
				t.Fatal("replay wrong classification", err)
			}
			if hostedObservationState(t, f, a) != before {
				t.Fatal("invalid replay mutated journal")
			}
		})
	}
}

func TestRetainedHistoricalCaptureAndQueryIgnoreDamagedTimer(t *testing.T) {
	f := supervisorFixture(t)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "history-timer", "hosted", 2, 1))
	in := retainTimerCooldown(t, f, a, 6*time.Minute)
	records, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	q, err := f.e.ReadHostedAllowanceQuery(t.Context(), attemptProject, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.ScheduleHostedCooldown(t.Context(), attemptProject, a.ID, 1); err != nil {
		t.Fatal(err)
	}
	f.e.schedulingNow = func() time.Time { return in.Observation.RetryNotBefore }
	f.start(t, attemptProject, Receipt{ID: a.OperationID})
	admissionSQL(t, f, "UPDATE review_operation_waits SET retry_not_before=? WHERE operation_id=?", in.Observation.RetryNotBefore.Add(time.Minute).Format(time.RFC3339Nano), a.OperationID)
	before := hostedObservationState(t, f, a)
	if _, err = f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID); err != nil {
		t.Fatal("capture blocked by timer", err)
	}
	if got, err := f.e.ReadHostedAllowanceQuery(t.Context(), attemptProject, a.ID); err != nil || got.ID != q.ID {
		t.Fatal("query blocked by timer", got, err)
	}
	if _, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, records[0].Input); err != nil {
		t.Fatal("exact replay blocked by timer", err)
	}
	fresh := records[0].Input
	fresh.CaptureSHA256 = strings.Repeat("f", 64)
	fresh.CaptureID = "review_capture_" + fresh.CaptureSHA256[:32]
	if _, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, fresh); err == nil {
		t.Fatal("fresh historical write accepted")
	}
	if hostedObservationState(t, f, a) != before {
		t.Fatal("historical read/replay changed state")
	}
}

func TestRetainedConfigurationStorageAndPredecessor(t *testing.T) {
	for _, damage := range []string{"blob", "predecessor"} {
		t.Run(damage, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			r := f.enqueue(t, attemptProject, "configuration-history", "cli", 2, 1)
			revision := int64(1)
			c := Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4, 6}, Channels: []string{"cli", "hosted"}}
			if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "history-next", c); err != nil {
				t.Fatal(err)
			}
			if damage == "blob" {
				admissionSQL(t, f, "UPDATE review_execution_config SET body_json=CAST(body_json AS BLOB) WHERE project_id=? AND revision=1", attemptProject)
				err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					_, err := sourceConfiguration(ctx, tx, attemptProject, 1)
					return err
				})
				if !isStoredUnavailable(err, storedConfigurationHistory) {
					t.Fatal("nontext source accepted", err)
				}
			} else {
				admissionSQL(t, f, "UPDATE review_execution_config SET body_json=json_set(body_json,'$.expected_revision',0) WHERE project_id=? AND revision=2", attemptProject)
				err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					o, err := readOperation(ctx, tx, attemptProject, r.ID)
					if err != nil {
						return err
					}
					_, err = compatibleQueuedConfiguration(ctx, tx, o, c, 2)
					return err
				})
				if !isStoredUnavailable(err, storedConfigurationHistory) {
					t.Fatal("inconsistent predecessor accepted", err)
				}
			}
		})
	}
}

func TestRetainedHeldOperationHTTPDoesNotLoadUnrequestedAllowance(t *testing.T) {
	f, a, in := cliAllowanceFixture(t)
	if err := f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, 1, "held", "allowance_exhausted", ""); err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, "UPDATE review_cli_preflight SET body_json='{' WHERE attempt_id=?", a.ID)
	for _, version := range []string{"", "1", "2", "3"} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+attemptProject+"/reviews/operations/"+a.OperationID, nil)
		r.Header.Set("Authorization", "Bearer "+f.reader)
		r.Header.Set(operationWaitHeader, version)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if version == "3" {
			if w.Code == http.StatusOK {
				t.Fatal("negotiated corruption hidden")
			}
		} else if w.Code != http.StatusOK {
			t.Fatal("legacy held read failed", version, w.Code, w.Body.String())
		}
	}
}

func TestRetainedDetachedQueryCannotReceiveNewConfirmation(t *testing.T) {
	f := supervisorFixture(t)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "middle-query", "hosted", 2, 1))
	var middle Attempt
	var query HostedAllowanceQuery
	for i := 1; i <= 3; i++ {
		in := retainTimerCooldown(t, f, a, time.Duration(i)*6*time.Minute)
		if i == 2 {
			middle = a
			var err error
			query, err = f.e.ReadHostedAllowanceQuery(t.Context(), attemptProject, a.ID)
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.e.ScheduleHostedCooldown(t.Context(), attemptProject, a.ID, 1); err != nil {
			t.Fatal(err)
		}
		if i < 3 {
			due := in.Observation.RetryNotBefore
			f.e.schedulingNow = func() time.Time { return due }
			a = f.start(t, attemptProject, Receipt{ID: a.OperationID})
		}
	}
	// An intact confirmed replay remains read-only even after detachment.
	if _, err := f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, middle.ID, 1, *query.Comment); err != nil {
		t.Fatal(err)
	}
	comment := *query.Comment
	query.Comment = nil
	query.Revision = 1
	admissionSQL(t, f, "UPDATE review_hosted_allowance_queries SET revision=1,body_json=? WHERE attempt_id=?", encoded(query), middle.ID)
	before := snapshotTable(t, f, "review_hosted_allowance_queries") + snapshotTable(t, f, "events")
	_, err := f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, middle.ID, 1, comment)
	attemptCode(t, err, "review_conflict")
	if snapshotTable(t, f, "review_hosted_allowance_queries")+snapshotTable(t, f, "events") != before {
		t.Fatal("detached confirmation mutated history")
	}
}
