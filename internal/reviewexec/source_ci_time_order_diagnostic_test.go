package reviewexec

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type sourceCITimeOrderError struct {
	sourceCIStageFailure
	issue string
}

func (e sourceCITimeOrderError) CIResultIssue() string { return e.issue }

func TestSourceCITimeOrderIssuePersistsWithoutPartialFactsOrRecollection(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	failure := sourceCITimeOrderError{sourceCIStageFailure{"head_results", &reviewprovider.Error{Code: "provider_invalid_response", Message: "PRIVATE provider field value", Status: 200}}, "check_run_time_order"}
	reader := &sourceCITestReader{ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		return sourceCITestObservation(target, "satisfied"), failure
	}}
	receipt := refreshEnqueue(t, f, "time-order-ci", sourceCIRequest())
	cancel, done := refreshStart(t, f, reader)
	got := refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	want := "ci_provider_invalid_response_head_results_check_run_time_order"
	if got.Observation == nil || got.CI == nil || got.CI.State != "unproven" || got.CI.Reason != want || got.CI.Required != 0 || got.CI.Satisfied != 0 {
		t.Fatal("invalid source inventory supplied partial credit", got.CI)
	}
	before := sourceCITestState(t, f)
	for range 2 {
		read := readSourceCI(t, f)
		if read.Reason != want || read.State != "unproven" || read.Required != 0 || read.Satisfied != 0 || refreshEnqueue(t, f, "time-order-ci", sourceCIRequest()) != receipt {
			t.Fatal("retained reason/replay changed facts", read)
		}
		body, _ := json.Marshal(read)
		if strings.Contains(string(body), "PRIVATE") {
			t.Fatal("provider detail leaked")
		}
	}
	if sourceCITestState(t, f) != before || reader.ciCalls.Load() != 1 {
		t.Fatal("diagnostic read/replay changed evidence or recollected provider")
	}
}

func TestSourceCITimeOrderIssueOnlyDecoratesInvalidResponse(t *testing.T) {
	for _, tc := range []struct{ code, issue, want string }{
		{"provider_invalid_response", "check_run_time_order", "ci_provider_invalid_response_head_results_check_run_time_order"},
		{"provider_invalid_response", "PRIVATE field", "ci_provider_invalid_response_head_results"},
		{"provider_rate_limited", "check_run_time_order", "ci_provider_rate_limited_head_results"},
		{"PRIVATE", "check_run_time_order", "ci_provider_unavailable_head_results"},
	} {
		failure := sourceCITimeOrderError{sourceCIStageFailure{"head_results", &reviewprovider.Error{Code: tc.code, Message: "PRIVATE"}}, tc.issue}
		if got := sourceCIErrorReason(t.Context(), failure); got != tc.want {
			t.Fatal("invented issue or changed provider class", got, tc.want)
		}
	}
}
