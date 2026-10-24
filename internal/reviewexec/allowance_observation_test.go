package reviewexec

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
	"net/http"
	"net/http/httptest"
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

func TestHeldAllowanceExplicitOrderHTTPReplayAndRestart(t *testing.T) {
	f := newAttemptFixture(t)
	explicitOrderConfigure(t, f)
	receipt := explicitOrderEnqueue(t, f, "held-explicit-choice", "cli", true, 1)
	a := f.start(t, attemptProject, receipt)
	in := CLIAllowanceObservation{Anchor: explicitOrderAnchor(), ObservedAt: stamp(), Remaining: 0, Limit: 3, FullCapacityInMS: 0, RollingWindowMS: 3600000, ChangedFiles: 1}
	if err := f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, 1, "held", "allowance_exhausted", ""); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/projects/" + attemptProject + "/reviews/operations/" + a.OperationID
	read := f.request("GET", path, f.writer, "", "")
	var original Operation
	if read.Code != http.StatusOK || json.Unmarshal(read.Body.Bytes(), &original) != nil || !original.AllowOutOfOrder || original.Allowance == nil || original.Allowance.Remaining == nil || *original.Allowance.Remaining != 0 {
		t.Fatal("explicit held result lost choice or known-zero facts", read.Code, read.Body.String())
	}
	body, _ := json.Marshal(explicitOrderRequest("cli", true))
	before := policyState(t, f)
	request := httptest.NewRequest("POST", "/api/v1/projects/"+attemptProject+"/reviews/requests", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+f.writer)
	request.Header.Set("Idempotency-Key", "held-explicit-choice")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Prefer", "wait=2")
	replay := httptest.NewRecorder()
	f.handler.ServeHTTP(replay, request)
	if replay.Code != http.StatusOK || !bytes.Equal(replay.Body.Bytes(), read.Body.Bytes()) || before != policyState(t, f) {
		t.Fatal("exact explicit replay changed original held facts or rows", replay.Code, replay.Body.String())
	}
	body, _ = json.Marshal(explicitOrderRequest("cli", false))
	changed := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/requests", f.writer, "held-explicit-choice", string(body))
	if changed.Code != http.StatusConflict || before != policyState(t, f) {
		t.Fatal("same key changed retained ordering intent", changed.Code)
	}
	explicitOrderReopen(t, f)
	op, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), a.OperationID)
	if err != nil || !reflect.DeepEqual(op, original) {
		t.Fatal("restart lost explicit held operation", op, err)
	}
	if err := ValidateAllowanceSnapshot(t.Context(), f.store.DB(), 7); err != nil {
		t.Fatal("schema-seven held snapshot rejected", err)
	}
}

// A later held classification does not change the immutable earlier positive
// observation into negative timing, nor make that valid source unreadable.
func TestHeldAllowancePositiveObservationRemainsReadable(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			var f *attemptFixture
			var a Attempt
			var replay func() error
			reason, table, key := "allowance_exhausted", "review_cli_preflight", "allowance-request"
			if channel == "cli" {
				var in CLIAllowanceObservation
				f, a, in = cliAllowanceFixture(t)
				replay = func() error { return f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, 1, in) }
			} else {
				var in HostedAllowanceInput
				f, a, _, in = hostedAllowanceFixture(t)
				replay = func() error { return f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, in) }
				reason, table, key = "hosted_allowance_cooldown", "review_hosted_preflight", "query-request"
			}
			if err := replay(); err != nil {
				t.Fatal(err)
			}
			if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
				t.Fatal(err)
			}
			bindPublisher := func() {
				var err error
				f.e, err = New(f.store, attemptCapabilities, WithNativeProjection())
				if err != nil {
					t.Fatal(err)
				}
				mux := http.NewServeMux()
				f.e.Register(mux)
				reviews.Register(mux, f.store)
				f.handler = mux
			}
			bindPublisher()
			metadata := projectionPull(2)
			if _, err := f.e.PublishAttempt(t.Context(), attemptProject, a.ID, &metadata); err != nil {
				t.Fatal("initial native publication", err)
			}
			var body string
			if err := f.store.DB().QueryRow(`SELECT body_json FROM `+table+` WHERE attempt_id=?`, a.ID).Scan(&body); err != nil {
				t.Fatal(err)
			}
			var err error
			a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", reason, "")
			if err != nil {
				t.Fatal("valid positive observation prevented held transition/publication", err)
			}
			_, projected := projectedAttempt(t, f, a)
			if projected.State != "held" || projected.Reason != reason {
				t.Fatal("native transition projection lost hold", projected)
			}
			before := attemptState(t, f, a) + policyState(t, f)
			path := "/api/v1/projects/" + attemptProject + "/reviews/operations/" + a.OperationID
			var original []byte
			for phase := 0; phase < 2; phase++ {
				if phase == 1 {
					cliAllowanceReopen(t, f)
					bindPublisher()
				}
				read := f.request("GET", path, f.reader, "", "")
				var op Operation
				if read.Code != http.StatusOK || json.Unmarshal(read.Body.Bytes(), &op) != nil || op.State != "held" || op.Reason != reason || op.Allowance != nil || bytes.Contains(read.Body.Bytes(), []byte(`"allowance":`)) {
					t.Fatal("positive observation became unreadable or negative timing", phase, read.Code, read.Body.String())
				}
				if phase == 0 {
					original = bytes.Clone(read.Body.Bytes())
				} else if !bytes.Equal(original, read.Body.Bytes()) {
					t.Fatal("reopen changed held operation")
				}
				if err := replay(); err != nil {
					t.Fatal("original positive replay after hold", err)
				}
				request := httptest.NewRequest("POST", "/api/v1/projects/"+attemptProject+"/reviews/requests", bytes.NewBufferString(encoded(Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: channel, HeadSHA: op.HeadSHA})))
				request.Header.Set("Authorization", "Bearer "+f.writer)
				request.Header.Set("Idempotency-Key", key)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Prefer", "wait=2")
				result := httptest.NewRecorder()
				f.handler.ServeHTTP(result, request)
				if result.Code != http.StatusOK || !bytes.Equal(result.Body.Bytes(), read.Body.Bytes()) {
					t.Fatal("exact request replay lost held result", phase, result.Code)
				}
				if err := ValidateAllowanceSnapshot(t.Context(), f.store.DB(), 7); err != nil {
					t.Fatal("valid retained positive snapshot", err)
				}
				var after string
				if err := f.store.DB().QueryRow(`SELECT body_json FROM `+table+` WHERE attempt_id=?`, a.ID).Scan(&after); err != nil || after != body || attemptState(t, f, a)+policyState(t, f) != before {
					t.Fatal("read/replay/reopen changed original facts", err)
				}
			}
			// Positive facts still undergo canonical and cross-owner validation
			// before the optional negative projection is omitted.
			for _, damage := range []string{"canonical", "actor"} {
				bad := body + " "
				if damage == "actor" {
					if channel == "cli" {
						var record cliAllowanceRecord
						if json.Unmarshal([]byte(body), &record) != nil {
							t.Fatal("fixture CLI record")
						}
						record.ActorID = "different-actor"
						bad = encoded(record)
					} else {
						var record hostedAllowanceRecord
						if json.Unmarshal([]byte(body), &record) != nil {
							t.Fatal("fixture Hosted record")
						}
						record.ActorID = "different-actor"
						bad = encoded(record)
					}
				}
				if _, err := f.store.DB().Exec(`UPDATE `+table+` SET body_json=? WHERE attempt_id=?`, bad, a.ID); err != nil {
					t.Fatal(err)
				}
				read := f.request("GET", path, f.reader, "", "")
				if read.Code != http.StatusInternalServerError || !bytes.Contains(read.Body.Bytes(), []byte(`"code":"storage_unavailable"`)) || ValidateAllowanceSnapshot(t.Context(), f.store.DB(), 7) == nil {
					t.Fatal("damaged positive observation bypassed validation", damage, read.Code)
				}
			}
		})
	}
}
