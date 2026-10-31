package reviewexec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"strings"
	"testing"
	"time"
)

func TestFileLimitOverrideHTTPStrictBooleanAndRetainedChoice(t *testing.T) {
	f := newAttemptFixture(t)
	explicitOrderConfigure(t, f)
	path := "/api/v1/projects/" + attemptProject + "/reviews/requests"
	base := `{"expected_configuration_revision":1,"pull_number":4,"channel":"cli","head_sha":"` + strings.Repeat("a", 40) + `"`
	for i, tail := range []string{`,"override_file_limit":null}`, `,"override_file_limit":"true"}`, `,"override_file_limit":1}`, `,"override_file_limit":0}`, `,"override_file_limit":{}}`, `,"override_file_limit":[]}`, `,"override_file_limit":true,"override_file_limit":false}`, `,"override_file_limit":true,"unknown":1}`} {
		before := policyState(t, f)
		response := f.request("POST", path, f.writer, fmt.Sprintf("invalid-order-%d", i), base+tail)
		if response.Code != 400 || before != policyState(t, f) {
			t.Fatal("malformed choice admitted or changed state", response.Code, response.Body.String())
		}
	}
	for i, tail := range []string{`}`, `,"override_file_limit":false}`, `,"override_file_limit":true}`} {
		response := f.request("POST", path, f.writer, fmt.Sprintf("valid-order-%d", i), base+tail)
		var receipt Receipt
		if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &receipt) != nil {
			t.Fatal(response.Code, response.Body.String())
		}
		detail := f.request("GET", "/api/v1/projects/"+attemptProject+"/reviews/operations/"+receipt.ID, f.reader, "", "")
		var operation Operation
		if detail.Code != 200 || json.Unmarshal(detail.Body.Bytes(), &operation) != nil || operation.OverrideFileLimit != (i == 2) || (i < 2 && strings.Contains(detail.Body.String(), "override_file_limit")) {
			t.Fatal("choice not faithfully retained", detail.Code, detail.Body.String())
		}
	}
	for _, token := range []string{f.reader, f.other, ""} {
		before := policyState(t, f)
		response := f.request("POST", path, token, "unauthorized-explicit", base+`,"override_file_limit":true}`)
		if response.Code == 202 || before != policyState(t, f) {
			t.Fatal("explicit choice changed authorization", response.Code)
		}
	}
}

func fileOverrideEnqueue(t *testing.T, f *attemptFixture, key, channel string, allow bool) (Receipt, Request) {
	t.Helper()
	request := Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: channel, HeadSHA: strings.Repeat("a", 40), OverrideFileLimit: allow}
	body, err := f.e.Enqueue(t.Context(), f.scope(t, attemptProject, f.writer, false), key, request)
	if err != nil {
		t.Fatal(err)
	}
	var r Receipt
	if json.Unmarshal(body, &r) != nil {
		t.Fatal("receipt")
	}
	return r, request
}
func TestFileLimitOverrideReplayRestartAndPredecessorUpgrade(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	scope := f.scope(t, attemptProject, f.writer, false)
	r, request := fileOverrideEnqueue(t, f, "original", "cli", false)
	original, err := f.e.Enqueue(t.Context(), scope, "original", request)
	if err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `ALTER TABLE review_operations DROP COLUMN override_file_limit`)
	restorePreCooldownAttemptSchema(t, f)
	admissionSQL(t, f, `UPDATE metadata SET value='7' WHERE key='schema:review_execution'`)
	old, err := readOperation(t.Context(), f.store.DB(), attemptProject, r.ID)
	if err != nil || old.OverrideFileLimit {
		t.Fatal(old, err)
	}
	explicitOrderReopen(t, f)
	replay, err := f.e.Enqueue(t.Context(), scope, "original", request)
	if err != nil || !bytes.Equal(original, replay) {
		t.Fatal("old receipt changed", err)
	}
	request.OverrideFileLimit = true
	_, err = f.e.Enqueue(t.Context(), scope, "original", request)
	attemptCode(t, err, "idempotency_conflict")
	r, request = fileOverrideEnqueue(t, f, "explicit", "cli", true)
	a := f.start(t, attemptProject, r)
	before := attemptState(t, f, a)
	explicitOrderReopen(t, f)
	op, err := f.e.ReadOperation(t.Context(), scope, r.ID)
	if err != nil || !op.OverrideFileLimit || attemptState(t, f, a) != before {
		t.Fatal("restart lost override", op, err)
	}
	_, err = f.e.Enqueue(t.Context(), scope, "explicit", request)
	if err != nil {
		t.Fatal(err)
	}
	request.OverrideFileLimit = false
	_, err = f.e.Enqueue(t.Context(), scope, "explicit", request)
	attemptCode(t, err, "idempotency_conflict")
}
func TestFileLimitOverrideOnlySkipsProjectCeiling(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, allow := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", channel, allow), func(t *testing.T) {
				f := newAttemptFixture(t)
				policy := policyInput(1)
				policy.MaxReviewFiles = 1
				policyConfigure(t, f, f.scope(t, attemptProject, f.admin, true), "ceiling", policy)
				f.configure(t, attemptProject, "test-capability", 0, false)
				receipt, _ := fileOverrideEnqueue(t, f, "oversize", channel, allow)
				a := f.start(t, attemptProject, receipt)
				anchor := attemptAnchor(attemptProject, 2)
				anchor.ParentSHA = anchor.BaseSHA
				if channel == "cli" {
					in := CLIAllowanceObservation{Anchor: anchor, ObservedAt: stamp(), Remaining: 1, Limit: 1, RollingWindowMS: 1, ChangedFiles: 2}
					err := f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, 1, in)
					if !allow {
						attemptCode(t, err, "review_held")
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					in.ChangedFiles = 0
					attemptCode(t, f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, 1, in), "validation_failed")
				} else {
					poster := reviewprovider.Author{ID: 12, Login: "poster", Kind: "User"}
					bot := reviewprovider.Author{ID: 30, Login: "synthetic-provider", Kind: "Bot"}
					q, _, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, poster, bot)
					if err != nil {
						t.Fatal(err)
					}
					q, err = f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, hostedQueryComment(t, q))
					if err != nil {
						t.Fatal(err)
					}
					hash := strings.Repeat("c", 64)
					in := HostedAllowanceInput{QueryID: q.ID, Anchor: anchor, ObservedAt: stamp(), ChangedFiles: 2, CaptureID: "review_capture_" + hash[:32], CaptureSHA256: hash, CaptureBytes: 1, Observation: reviewprovider.HostedAllowanceObservation{State: "available_observed", Reason: "provider_available_observed", QueryCommentID: q.Comment.ID, ResponseCommentID: 92, QueryCreatedAt: q.Comment.CreatedAt, ResponseCreatedAt: q.Comment.CreatedAt.Add(time.Nanosecond)}}
					err = f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, in)
					if !allow {
						attemptCode(t, err, "review_held")
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: anchor.CandidateSHA})
					if err != nil {
						t.Fatal(err)
					}
					a, err = f.e.Admit(t.Context(), attemptProject, a.ID, 1, anchor, intent)
					if err != nil {
						t.Fatal(err)
					}
					plan := hostedActionSource(a)
					if err = f.e.VerifyHostedAction(t.Context(), attemptProject, a.ID, a.Revision, plan); err != nil {
						t.Fatal("final action lost override", err)
					}
					plan.Pull.HeadSHA = strings.Repeat("f", 40)
					attemptCode(t, f.e.VerifyHostedAction(t.Context(), attemptProject, a.ID, a.Revision, plan), "review_held")
				}
				control, err := f.e.ReadRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), 2, channel)
				if err != nil {
					t.Fatal(err)
				}
				one := control.Revision
				_, err = f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "closed", 2, channel, RoundControlRequest{ExpectedRevision: &one, Action: "close"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.e.ExecutionContext(t.Context(), attemptProject, a.OperationID); err == nil {
					t.Fatal("override bypassed closed rounds")
				}
				p, err := f.e.ReadPolicy(t.Context(), f.scope(t, attemptProject, f.admin, true))
				if err != nil || p.Policy.MaxReviewFiles != 1 {
					t.Fatal("override changed policy", err)
				}
			})
		}
	}
}
