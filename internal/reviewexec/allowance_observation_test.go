package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/benhook1013/FireController/internal/core"
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
			r := httptest.NewRequest("GET", "/api/v1/projects/"+attemptProject+"/reviews/operations/"+a.OperationID, nil)
			r.Header.Set("Authorization", "Bearer "+f.reader)
			r.Header.Set(operationWaitHeader, "3")
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
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
			compactHTTP := statusHTTP(t, f, a.OperationID, "", "wait=0", "3")
			var compact OperationStatus
			if compactHTTP.Code != 200 || json.Unmarshal(compactHTTP.Body.Bytes(), &compact) != nil || compact.State != "held" || compact.Reason != reason || len(compactHTTP.Body.Bytes()) > 1024 {
				t.Fatal("compact held status unavailable or expanded", compactHTTP.Code, compactHTTP.Body.String())
			}
			wantRetry := ""
			if channel == "hosted" {
				wantRetry = op.Allowance.RetryNotBefore
			}
			if compact.RetryNotBefore != wantRetry || strings.Contains(compactHTTP.Body.String(), `"allowance"`) || strings.Contains(compactHTTP.Body.String(), `"remaining"`) {
				t.Fatal("compact cooldown lost deadline or loaded full allowance output", compactHTTP.Body.String())
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
			if channel == "hosted" && statusHTTP(t, f, a.OperationID, "", "").Code != 500 {
				t.Fatal("compact read exposed an unvalidated deadline")
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

func TestHeldAllowanceOnlyMissingPreflightIsCompatible(t *testing.T) {
	ctx := context.Background()
	f0, a0, _ := cliAllowanceFixture(t)
	if _, err := f0.e.Advance(ctx, attemptProject, a0.ID, a0.Revision, "held", "allowance_exhausted", ""); err != nil {
		t.Fatal(err)
	}
	op0, err := readOperationBase(ctx, f0.store.DB(), attemptProject, a0.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readCLIAllowanceRecord(ctx, f0.store.DB(), op0, a0); !errors.Is(err, errAllowanceObservationAbsent) || !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("absent top-level preflight lost its compatibility sentinel", err)
	}
	if allowance, err := readHeldAllowance(ctx, f0.store.DB(), op0); err != nil || allowance != nil {
		t.Fatal("absent preflight was not treated as legacy no-facts", allowance, err)
	}

	f, a, input := cliAllowanceFixture(t)
	input.Remaining = 0
	if err := f.e.RecordCLIExhaustion(ctx, attemptProject, a.ID, a.Revision, input); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(ctx, attemptProject, a.ID, a.Revision, "held", "allowance_exhausted", ""); err != nil {
		t.Fatal(err)
	}
	op, err := readOperationBase(ctx, f.store.DB(), attemptProject, a.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	deleteRelatedRowWithoutForeignKeys(t, f.store.DB(), "projects", "id", attemptProject)
	if _, err := readHeldAllowance(ctx, f.store.DB(), op); err == nil {
		t.Fatal("missing related project row was hidden as no allowance")
	} else {
		attemptCode(t, err, "storage_unavailable")
	}
	if err := ValidateAllowanceSnapshot(ctx, f.store.DB(), 10); err == nil {
		t.Fatal("snapshot validation accepted missing related project")
	} else {
		attemptCode(t, err, "storage_unavailable")
	}
}

func TestHeldHostedAllowanceMissingQueryIsUnavailable(t *testing.T) {
	ctx := context.Background()
	f, a, input := cooldownObservationFixture(t)
	if err := f.e.RecordHostedCooldown(ctx, attemptProject, a.ID, a.Revision, input); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(ctx, attemptProject, a.ID, a.Revision, "held", "hosted_allowance_cooldown", ""); err != nil {
		t.Fatal(err)
	}
	op, err := readOperationBase(ctx, f.store.DB(), attemptProject, a.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	deleteRelatedRowWithoutForeignKeys(t, f.store.DB(), "review_hosted_allowance_queries", "attempt_id", a.ID)
	if _, err := readHeldAllowance(ctx, f.store.DB(), op); err == nil {
		t.Fatal("missing hosted query was hidden as no allowance")
	} else {
		attemptCode(t, err, "storage_unavailable")
	}
	if err := ValidateAllowanceSnapshot(ctx, f.store.DB(), 10); err == nil {
		t.Fatal("snapshot validation accepted missing hosted query")
	} else {
		attemptCode(t, err, "storage_unavailable")
	}
}

func deleteRelatedRowWithoutForeignKeys(t *testing.T, db *sql.DB, table, column, key string) {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `DELETE FROM `+table+` WHERE `+column+`=?`, key); err != nil {
		t.Fatal("delete related fixture row", table, err)
	}
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
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

func TestHeldAllowanceLegacyPolicyZeroWithoutFacts(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "pre-policy-no-facts", channel, 2, 1))
			reason := "allowance_exhausted"
			table := "review_cli_preflight"
			if channel == "hosted" {
				reason, table = "hosted_allowance_cooldown", "review_hosted_preflight"
			}
			var err error
			a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", reason, "")
			if err != nil {
				t.Fatal(err)
			}
			// Execution schema v3 adds this column with default zero to older rows.
			if _, err := f.store.DB().Exec(`UPDATE review_operations SET policy_revision=0 WHERE id=?`, a.OperationID); err != nil {
				t.Fatal(err)
			}
			before := attemptState(t, f, a)
			check := func() {
				t.Helper()
				w := f.request("GET", "/api/v1/projects/"+attemptProject+"/reviews/operations/"+a.OperationID, f.reader, "", "")
				var o Operation
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &o) != nil || o.PolicyRevision != 0 || o.State != "held" || o.Reason != reason || o.Allowance != nil {
					t.Fatal("legacy operation without optional facts became unreadable", w.Code, w.Body.String())
				}
				if attemptState(t, f, a) != before {
					t.Fatal("legacy read rewrote retained history")
				}
			}
			check()
			cliAllowanceReopen(t, f)
			mux := http.NewServeMux()
			f.e.Register(mux)
			f.handler = mux
			check()
			// Absence is compatible; a present malformed record must still fail closed.
			if _, err := f.store.DB().Exec(`INSERT INTO `+table+`(project_id,attempt_id,body_json) VALUES(?,?,?)`, attemptProject, a.ID, `{}`); err != nil {
				t.Fatal(err)
			}
			if w := f.request("GET", "/api/v1/projects/"+attemptProject+"/reviews/operations/"+a.OperationID, f.reader, "", ""); w.Code != 500 {
				t.Fatal("present invalid facts were hidden", w.Code, w.Body.String())
			}
		})
	}
}

func TestHeldAllowanceHTTPNegotiationPreservesLegacyShapes(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			var f *attemptFixture
			var a Attempt
			var err error
			reason, key := "allowance_exhausted", "allowance-request"
			if channel == "cli" {
				var in CLIAllowanceObservation
				f, a, in = cliAllowanceFixture(t)
				in.Remaining = 0
				err = f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, 1, in)
			} else {
				var in HostedAllowanceInput
				f, a, in = cooldownObservationFixture(t)
				reason, key = "hosted_allowance_cooldown", "query-request"
				err = f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, in)
			}
			if err != nil {
				t.Fatal(err)
			}
			a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", reason, "")
			if err != nil {
				t.Fatal(err)
			}
			before := attemptState(t, f, a)
			counts := attemptCount(t, f.store, "SELECT count(*) FROM receipts")
			body, err := json.Marshal(Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: channel, HeadSHA: strings.Repeat("a", 40)})
			if err != nil {
				t.Fatal(err)
			}
			for _, version := range []string{"", "1", "2", "3", "4", "unknown"} {
				for _, kind := range []string{"full", "status", "enqueue-replay"} {
					t.Run(version+"/"+kind, func(t *testing.T) {
						path := "/api/v1/projects/" + attemptProject + "/reviews/operations/" + a.OperationID
						method, token, payload := "GET", f.reader, ""
						if kind == "status" {
							path += "/status"
						}
						if kind == "enqueue-replay" {
							method, token, payload, path = "POST", f.writer, string(body), "/api/v1/projects/"+attemptProject+"/reviews/requests"
						}
						r := httptest.NewRequest(method, path, strings.NewReader(payload))
						r.Header.Set("Authorization", "Bearer "+token)
						r.Header.Set("Prefer", "wait=1")
						if version != "" {
							r.Header.Set(operationWaitHeader, version)
						}
						if method == "POST" {
							r.Header.Set("Idempotency-Key", key)
						}
						w := httptest.NewRecorder()
						f.handler.ServeHTTP(w, r)
						var fields map[string]json.RawMessage
						if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &fields) != nil {
							t.Fatal("held response", w.Code, w.Body.String())
						}
						_, allowance := fields["allowance"]
						_, retry := fields["retry_not_before"]
						wantAllowance := version == "3" && kind != "status"
						wantRetry := version == "3" && kind == "status" && channel == "hosted"
						if allowance != wantAllowance || retry != wantRetry {
							t.Fatal("unnegotiated held fields or missing version3 facts", version, kind, w.Body.String())
						}
						if !strings.Contains(w.Header().Get("Vary"), operationWaitHeader) && !strings.Contains(strings.Join(w.Header().Values("Vary"), ","), operationWaitHeader) {
							t.Fatal("missing negotiation cache boundary")
						}
					})
				}
			}
			o, err := f.e.ReadOperation(t.Context(), operationReadScope(t, f), a.OperationID)
			if err != nil || o.Allowance == nil || before != attemptState(t, f, a) || counts != attemptCount(t, f.store, "SELECT count(*) FROM receipts") {
				t.Fatal("HTTP negotiation changed engine facts or replayed mutations", o, err)
			}
		})
	}
}

func TestStoredUnavailableClassificationPreservesPublicErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		makeError func() error
		kind      storedUnavailableKind
	}{
		{"waiting intent", waitUnavailable, storedWaitingIntent},
		{"allowance", allowanceUnavailable, storedAllowanceObservation},
		{"hosted query", hostedQueryUnavailable, storedHostedQuery},
		{"hosted observation", hostedAllowanceUnavailable, storedHostedObservation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.makeError()
			wrapped := fmt.Errorf("retained evidence: %w", err)
			if !isStoredUnavailable(wrapped, tc.kind) || isStoredUnavailable(wrapped, tc.kind+1) {
				t.Fatal("typed classification lost through wrapping", wrapped)
			}
			var failure *core.Error
			if !errors.As(wrapped, &failure) || failure.Code != "storage_unavailable" || failure.Status != 500 || failure.Message != err.Error() {
				t.Fatal("public error contract changed", failure)
			}
			recorder := httptest.NewRecorder()
			core.WriteError(recorder, wrapped)
			var body struct {
				Error struct{ Code, Message string }
			}
			if recorder.Code != 500 || json.Unmarshal(recorder.Body.Bytes(), &body) != nil || body.Error.Code != failure.Code || body.Error.Message != failure.Message {
				t.Fatal("typed classification escaped the public response", recorder.Code, recorder.Body.String())
			}
			// Human-facing wording is deliberately not classification authority.
			failure.Message = "Reworded retained evidence diagnostic."
			if !isStoredUnavailable(wrapped, tc.kind) {
				t.Fatal("wording edit changed classification")
			}
			lookalike := core.Fail("storage_unavailable", failure.Message, 500)
			if isStoredUnavailable(lookalike, tc.kind) || interruptedAllowanceUnavailable(lookalike) {
				t.Fatal("actual storage error acquired retained-evidence classification")
			}
		})
	}
}

func TestStoredUnavailableDependencyAndRecoveryClassification(t *testing.T) {
	query := fmt.Errorf("lookup: %w", newStoredUnavailable(storedHostedQuery, "Reworded query diagnostic."))
	if !storedAllowanceUnavailable(allowanceDependencyError(query)) {
		t.Fatal("typed missing query did not become unusable allowance")
	}
	storage := core.Fail("storage_unavailable", "Stored hosted allowance query is unavailable.", 500)
	if allowanceDependencyError(storage) != storage || storedAllowanceUnavailable(storage) {
		t.Fatal("storage lookup failure was reclassified by wording")
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, errors.New("database failed")} {
		if allowanceDependencyError(err) != err || interruptedAllowanceUnavailable(err) {
			t.Fatal("real lookup failure was suppressed", err)
		}
	}
	for _, err := range []error{allowanceUnavailable(), newStoredUnavailable(storedHostedObservation, "Reworded hosted observation diagnostic.")} {
		if !interruptedAllowanceUnavailable(fmt.Errorf("recovery: %w", err)) {
			t.Fatal("unusable optional evidence did not allow safe cancellation", err)
		}
	}
	if !isWaitUnavailable(fmt.Errorf("history: %w", newStoredUnavailable(storedWaitingIntent, "Reworded history diagnostic."))) || isWaitUnavailable(core.Fail("storage_unavailable", "Stored review waiting intent is unavailable.", 500)) {
		t.Fatal("history classification still depends on message text")
	}
}
