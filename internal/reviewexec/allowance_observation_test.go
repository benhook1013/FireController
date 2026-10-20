package reviewexec

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func cooldownObservationFixture(t *testing.T) (*attemptFixture, Attempt, HostedAllowanceInput) {
	t.Helper()
	f, a, q, in := hostedAllowanceFixture(t)
	in.Observation.State, in.Observation.Reason = "cooldown_observed", "provider_cooldown_observed"
	in.Observation.RetryNotBefore = in.Observation.ResponseCreatedAt.Add(3 * time.Minute)
	observed, _ := time.Parse(time.RFC3339Nano, in.ObservedAt)
	start, _ := time.Parse(time.RFC3339Nano, q.EarliestPosting)
	_, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, HostedObservationInput{Kind: "allowance_query", CaptureID: in.CaptureID, CaptureSHA256: in.CaptureSHA256, CaptureBytes: in.CaptureBytes, StartedAt: start.Truncate(time.Second).Format(time.RFC3339Nano), FinishedAt: in.ObservedAt, ObservedAt: observed.Format(time.RFC3339Nano), ExpectedAnchor: q.Anchor, Intent: q.Intent})
	if err != nil {
		t.Fatal(err)
	}
	return f, a, in
}

func TestHeldAllowanceSQLReadHTTPReplayAndValidation(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			var f *attemptFixture
			var a Attempt
			var err error
			reason := "allowance_exhausted"
			var input CLIAllowanceObservation
			var hosted HostedAllowanceInput
			if channel == "cli" {
				f, a, input = cliAllowanceFixture(t)
				input.Remaining, input.FullCapacityInMS = 0, 0
				if err = f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, 1, input); err == nil {
					t.Fatal("zero observation accepted as positive preflight")
				}
				err = f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, 1, input)
			} else {
				f, a, hosted = cooldownObservationFixture(t)
				reason = "hosted_allowance_cooldown"
				if err = f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, hosted); err == nil {
					t.Fatal("cooldown accepted as positive preflight")
				}
				err = f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, hosted)
			}
			if err != nil {
				t.Fatal(err)
			}
			a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", reason, "")
			if err != nil {
				t.Fatal(err)
			}
			original := attemptState(t, f, a)
			var before string
			table := "review_cli_preflight"
			if channel == "hosted" {
				table = "review_hosted_preflight"
			}
			if err := f.store.DB().QueryRow(`SELECT body_json FROM `+table+` WHERE attempt_id=?`, a.ID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			w := f.request("GET", "/api/v1/projects/"+attemptProject+"/reviews/operations/"+a.OperationID, f.reader, "", "")
			if w.Code != http.StatusOK {
				t.Fatal("held HTTP read", w.Code, w.Body.String())
			}
			var op Operation
			if err = json.Unmarshal(w.Body.Bytes(), &op); err != nil {
				t.Fatal(err)
			}
			if op.Allowance == nil || op.State != "held" || op.Reason != reason {
				t.Fatal("missing held observation", op)
			}
			if channel == "cli" {
				v := op.Allowance
				if v.Kind != "cli_exhausted" || v.Remaining == nil || *v.Remaining != 0 || v.Limit == nil || *v.Limit != input.Limit || v.FullCapacityInMS == nil || *v.FullCapacityInMS != 0 || v.RollingWindowMS == nil || v.RetryNotBefore != "" || !bytes.Contains(w.Body.Bytes(), []byte(`"full_capacity_in_ms":0`)) {
					t.Fatal("lost known zeros or fabricated retry", v)
				}
				err = f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, 1, input)
			} else {
				v := op.Allowance
				if v.Kind != "hosted_cooldown" || v.RetryNotBefore != hosted.Observation.RetryNotBefore.Format(time.RFC3339Nano) || v.Remaining != nil || v.Limit != nil || v.FullCapacityInMS != nil || v.RollingWindowMS != nil {
					t.Fatal("wrong cooldown projection", v)
				}
				err = f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, hosted)
				if _, err := f.e.ReadHostedAllowance(t.Context(), attemptProject, a.ID); err == nil {
					t.Fatal("cooldown returned as positive allowance proof")
				}
			}
			if err != nil {
				t.Fatal("immutable held replay", err)
			}
			if err := ValidateAllowanceSnapshot(t.Context(), f.store.DB(), 6); err != nil {
				t.Fatal("typed retained snapshot", err)
			}
			cliAllowanceReopen(t, f)
			read, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), a.OperationID)
			if err != nil || !reflect.DeepEqual(read.Allowance, op.Allowance) {
				t.Fatal("restart lost allowance", read, err)
			}
			var after string
			if err := f.store.DB().QueryRow(`SELECT body_json FROM `+table+` WHERE attempt_id=?`, a.ID).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before || attemptState(t, f, a) != original {
				t.Fatal("read/replay/restart rewrote source")
			}
			if w := f.request("GET", "/api/v1/projects/"+otherAttemptProject+"/reviews/operations/"+a.OperationID, f.other, "", ""); w.Code == 200 {
				t.Fatal("foreign project exposed observation")
			}
			if _, err := f.store.DB().Exec(`UPDATE `+table+` SET body_json=body_json||' ' WHERE attempt_id=?`, a.ID); err != nil {
				t.Fatal(err)
			}
			if err := ValidateAllowanceSnapshot(t.Context(), f.store.DB(), 6); err == nil {
				t.Fatal("noncanonical retained body accepted")
			}
			if _, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), a.OperationID); err == nil {
				t.Fatal("corrupt optional fact silently accepted")
			}
		})
	}
}

func TestHeldAllowanceLegacyMissingAndPositiveBytes(t *testing.T) {
	f, a, in := cliAllowanceFixture(t)
	if err := f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_cli_preflight WHERE attempt_id=?`, a.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var record cliAllowanceRecord
	if json.Unmarshal([]byte(body), &record) != nil || encoded(record) != body {
		t.Fatal("legacy positive encoding")
	}
	op, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), a.OperationID)
	if err != nil || op.Allowance != nil {
		t.Fatal("positive preparing observation leaked as held facts", op, err)
	}
	// Historical validation is structural, not an ingestion freshness test.
	record.Observation.ObservedAt = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	record.Observation.Anchor.ObservedAt = record.Observation.ObservedAt
	if _, err := f.store.DB().Exec(`UPDATE review_cli_preflight SET body_json=? WHERE attempt_id=?`, encoded(record), a.ID); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAllowanceSnapshot(t.Context(), f.store.DB(), 6); err != nil {
		t.Fatal("historical positive observation rejected as stale", err)
	}
	f2 := newAttemptFixture(t)
	f2.configure(t, attemptProject, "test-capability", 0, false)
	b := f2.start(t, attemptProject, f2.enqueue(t, attemptProject, "legacy-no-facts", "cli", 2, 1))
	if _, err := f2.e.Advance(t.Context(), attemptProject, b.ID, 1, "held", "allowance_exhausted", ""); err != nil {
		t.Fatal(err)
	}
	old, err := f2.e.ReadOperation(context.Background(), f2.scope(t, attemptProject, f2.writer, false), b.OperationID)
	if err != nil || old.Allowance != nil {
		t.Fatal("missing legacy observation became zero", old, err)
	}
}

func TestCooldownCannotAuthorizeActionAndRequiresRetainedQueryReference(t *testing.T) {
	f, a, in := cooldownObservationFixture(t)
	if _, err := f.store.DB().Exec(`DELETE FROM review_hosted_observations WHERE attempt_id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, in); err == nil {
		t.Fatal("cooldown without raw-source journal accepted")
	}
	f, a, in = cooldownObservationFixture(t)
	if err := f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal(err)
	}
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: in.Anchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, 1, in.Anchor, intent)
	if err != nil {
		t.Fatal(err)
	}
	source := hostedActionSource(a)
	if err := f.e.VerifyHostedAction(t.Context(), attemptProject, a.ID, a.Revision, source); err == nil {
		t.Fatal("cooldown became a positive action grant")
	}
}

func TestHeldAllowancePreparingRevisionProvenance(t *testing.T) {
	f, a, in := cliAllowanceFixture(t)
	in.Remaining = 0
	if err := f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, 2, in); err == nil {
		t.Fatal("noninitial preparing revision accepted")
	}
	if err := f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, 1, "held", "allowance_exhausted", ""); err != nil {
		t.Fatal(err)
	}
	// Re-encode canonically, so the provenance check, not whitespace, rejects it.
	var body string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_cli_preflight WHERE attempt_id=?`, a.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var record cliAllowanceRecord
	if json.Unmarshal([]byte(body), &record) != nil {
		t.Fatal("record")
	}
	record.AttemptRevision = 2
	if _, err := f.store.DB().Exec(`UPDATE review_cli_preflight SET body_json=? WHERE attempt_id=?`, encoded(record), a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), a.OperationID); err == nil {
		t.Fatal("held revision masqueraded as preparing observation")
	}
	if err := ValidateAllowanceSnapshot(t.Context(), f.store.DB(), 6); err == nil {
		t.Fatal("backup accepted wrong preparing revision")
	}
}

func TestHeldAllowanceNegativeWriteEventFailureRollsBack(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			var f *attemptFixture
			var a Attempt
			var input CLIAllowanceObservation
			var hosted HostedAllowanceInput
			if channel == "cli" {
				f, a, input = cliAllowanceFixture(t)
				input.Remaining = 0
			} else {
				f, a, hosted = cooldownObservationFixture(t)
			}
			admissionSQL(t, f, `CREATE TRIGGER reject_negative_preflight BEFORE INSERT ON events WHEN NEW.event_type='review.`+channel+`_preflight' BEGIN SELECT RAISE(ABORT,'synthetic private diagnostic'); END`)
			before := attemptState(t, f, a)
			var err error
			if channel == "cli" {
				err = f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, 1, input)
			} else {
				err = f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, hosted)
			}
			if err == nil || strings.Contains(err.Error(), "synthetic") || attemptState(t, f, a) != before || attemptCount(t, f.store, `SELECT count(*) FROM review_`+channel+`_preflight`) != 0 {
				t.Fatal("negative observation/event did not roll back safely", err)
			}
		})
	}
}
