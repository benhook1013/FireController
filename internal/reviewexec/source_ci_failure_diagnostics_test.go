package reviewexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestSourceCIFailureDiagnosticsRemainBoundedAndUnproven(t *testing.T) {
	plan := SourcePlan{
		Repository: reviewprovider.Repository{ID: 801, FullName: "owner/repo"},
		Pull:       reviewprovider.Pull{Number: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main"},
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	cases := []struct {
		name, code string
		status     int
		cause      error
		want       string
	}{
		{name: "unknown", cause: errors.New("PRIVATE token URL"), want: "ci_provider_unavailable"},
		{name: "unknown_code", code: "PRIVATE_url_token", status: 503, want: "ci_provider_unavailable"},
		{name: "rate_limit", code: "provider_rate_limited", status: 429, want: "ci_provider_rate_limited"},
		{name: "authorization", code: "provider_unauthorized", status: 403, want: "ci_provider_unauthorized"},
		{name: "missing", code: "provider_not_found", status: 404, want: "ci_provider_not_found"},
		{name: "observation_changed", code: "provider_observation_changed", status: 200, want: "ci_provider_observation_changed"},
		{name: "invalid_response", code: "provider_invalid_response", status: 200, want: "ci_provider_invalid_response"},
		{name: "http_timeout", code: "provider_unavailable", status: 408, want: "ci_provider_timeout"},
		{name: "server", code: "provider_unavailable", status: 503, want: "ci_provider_server_error"},
		{name: "redirect", code: "provider_unavailable", status: 302, want: "ci_provider_redirect"},
		{name: "transport_unknown", code: "provider_unavailable", want: "ci_provider_unavailable"},
		{name: "deadline", cause: fmt.Errorf("PRIVATE: %w", context.DeadlineExceeded), want: "ci_collection_deadline"},
		{name: "cancellation", cause: fmt.Errorf("PRIVATE: %w", context.Canceled), want: "ci_collection_canceled"},
		{name: "parent_deadline", code: "provider_unavailable", want: "ci_collection_deadline"},
		{name: "parent_cancellation", code: "provider_unavailable", want: "ci_collection_canceled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			if tc.name == "parent_deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			} else if tc.name == "parent_cancellation" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			failure := tc.cause
			if failure == nil {
				failure = fmt.Errorf("PRIVATE wrapper: %w", &reviewprovider.Error{Code: tc.code, Message: "PRIVATE token URL", Status: tc.status})
			}
			reader := &sourceCITestReader{ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				// Even a partial passing inventory accompanying an error cannot credit CI.
				return sourceCITestObservation(target, "satisfied"), failure
			}}
			got := observeSourceCI(ctx, reader, plan)
			if got.State != "unproven" || got.Reason != tc.want || got.Results != nil || got.Actions != nil || got.Assessment.State != "" || !validSourceCI(*got, sourcePlanObservation(plan)) || reader.ciCalls.Load() != 1 {
				t.Fatalf("unsafe failed collection: %+v", got)
			}
			body, _ := json.Marshal(got)
			if strings.Contains(string(body), "PRIVATE") || len(body) > 2048 {
				t.Fatal("unbounded or private error retained")
			}
		})
	}
}

func TestSourceCIFailureDiagnosticsPersistWithoutRecollection(t *testing.T) {
	for _, code := range []string{"provider_rate_limited", "provider_unavailable"} {
		t.Run(code, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			reader := &sourceCITestReader{ci: func(context.Context, reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				return reviewprovider.CIObservation{}, &reviewprovider.Error{Code: code, Message: "PRIVATE"}
			}}
			receipt := refreshEnqueue(t, f, "failed-ci", sourceCIRequest())
			cancel, done := refreshStart(t, f, reader)
			got := refreshWait(t, f, receipt.ID, "completed")
			refreshStop(t, cancel, done)
			want := "ci_" + code
			if got.Observation == nil || got.CI == nil || got.CI.State != "unproven" || got.CI.Reason != want || got.CI.Required != 0 {
				t.Fatal("source lost or failed CI credited", got)
			}
			before := sourceCITestState(t, f)
			for range 2 {
				read := refreshRead(t, f, receipt.ID)
				assessment := readSourceCI(t, f)
				if read.CI.Reason != want || assessment.Reason != want || refreshEnqueue(t, f, "failed-ci", sourceCIRequest()) != receipt {
					t.Fatal("retained reason or original receipt changed")
				}
			}
			if sourceCITestState(t, f) != before || reader.ciCalls.Load() != 1 {
				t.Fatal("reads/replay rewrote history or repeated provider collection")
			}
		})
	}
}
