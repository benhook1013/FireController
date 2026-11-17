package reviewruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type classifiedActivityProvider struct {
	*driverHostedProvider
	observation func() reviewprovider.HostedActivity
	failure     error
}

func (p *classifiedActivityProvider) ObserveHostedActivity(ctx context.Context, repository, head string, selector reviewprovider.HostedActivitySelector) (reviewprovider.HostedActivity, error) {
	if _, err := p.driverHostedProvider.ObserveHostedActivity(ctx, repository, head, selector); err != nil {
		return reviewprovider.HostedActivity{}, err
	}
	return p.observation(), p.failure
}

func activityObservation(state, reason string, offset time.Duration) func() reviewprovider.HostedActivity {
	return func() reviewprovider.HostedActivity {
		return reviewprovider.HostedActivity{State: state, Reason: reason, ObservedAt: time.Now().UTC().Add(offset)}
	}
}

type activityDiagnosticCase struct {
	name        string
	observation func() reviewprovider.HostedActivity
	failure     error
	want        string
}

func activityDiagnosticCases() []activityDiagnosticCase {
	idle := activityObservation("idle", "provider_activity_idle", 0)
	return []activityDiagnosticCase{
		{"rate limited", idle, &reviewprovider.Error{Code: "provider_rate_limited", Status: 429, Message: "PRIVATE_PROVIDER_BODY"}, "provider_activity_rate_limited"},
		{"unauthorized", idle, &reviewprovider.Error{Code: "provider_unauthorized", Status: 403, Message: "PRIVATE_PROVIDER_BODY"}, "provider_activity_unauthorized"},
		{"not found", idle, &reviewprovider.Error{Code: "provider_not_found", Status: 404, Message: "PRIVATE_PROVIDER_BODY"}, "provider_activity_not_found"},
		{"invalid response", idle, &reviewprovider.Error{Code: "provider_invalid_response", Status: 200, Message: "PRIVATE_PROVIDER_BODY"}, "provider_activity_response_invalid"},
		{"unavailable", idle, fmt.Errorf("PRIVATE_WRAPPER: %w", &reviewprovider.Error{Code: "provider_unavailable", Status: 503, Message: "PRIVATE_PROVIDER_BODY"}), "provider_activity_unavailable"},
		{"timeout", idle, context.DeadlineExceeded, "provider_activity_unavailable"},
		{"unknown typed error", idle, &reviewprovider.Error{Code: "PRIVATE_UNKNOWN_CODE", Status: 429, Message: "PRIVATE_PROVIDER_BODY"}, "provider_activity_unproven"},
		{"unknown error", idle, errors.New("provider_rate_limited PRIVATE_RAW_ERROR"), "provider_activity_unproven"},
		{"ambiguous", activityObservation("unproven", "activity_ambiguous", 0), nil, "provider_activity_ambiguous"},
		{"unknown state", activityObservation("unproven", "activity_state_unknown", 0), nil, "provider_activity_state_unknown"},
		{"other unproven", activityObservation("unproven", "PRIVATE_UNKNOWN_REASON", 0), nil, "provider_activity_unproven"},
		{"missing time", func() reviewprovider.HostedActivity { return reviewprovider.HostedActivity{State: "idle"} }, nil, "provider_activity_observation_invalid"},
		{"future time", activityObservation("idle", "provider_activity_idle", 6*time.Second), nil, "provider_activity_observation_invalid"},
		{"stale time", activityObservation("idle", "provider_activity_idle", -2*time.Minute), nil, "provider_activity_observation_invalid"},
		{"non UTC time", func() reviewprovider.HostedActivity {
			return reviewprovider.HostedActivity{State: "idle", ObservedAt: time.Now().In(time.FixedZone("PRIVATE_ZONE", 3600))}
		}, nil, "provider_activity_observation_invalid"},
		{"active", activityObservation("active", "provider_activity_active", 0), nil, "provider_review_active"},
		{"idle", idle, nil, ""},
		{"complete no signal", activityObservation("idle", "no_provider_activity_observed", 0), nil, ""},
	}
}

func TestHostedActivityDiagnosticClassificationIsBoundedAndPrivate(t *testing.T) {
	f := newHostedDriverFixture(t)
	plan := reviewexec.SourcePlan{Repository: f.provider.repository, Pull: f.provider.pulls[2]}
	for _, tc := range activityDiagnosticCases() {
		t.Run(tc.name, func(t *testing.T) {
			f.hosted.provider = &classifiedActivityProvider{f.provider, tc.observation, tc.failure}
			err := f.hosted.checkIdle(t.Context(), plan)
			if tc.want == "" {
				if err != nil {
					t.Fatal("idle observation changed admission", err)
				}
				return
			}
			var refusal *core.Error
			if !errors.As(err, &refusal) || refusal.Code != "review_held" || refusal.Details.(map[string]string)["reason"] != tc.want {
				t.Fatal("original activity refusal was collapsed", err)
			}
			encoded, _ := json.Marshal(refusal)
			if strings.Contains(string(encoded), "PRIVATE") || strings.Contains(string(encoded), "429") || strings.Contains(string(encoded), "https://") {
				t.Fatal("raw provider details leaked into activity hold", string(encoded))
			}
		})
	}
	if f.provider.queries != 0 || f.provider.reviews != 0 {
		t.Fatal("activity diagnostics performed provider mutation")
	}
}

func TestHostedActivityDiagnosticNativeInitialAndFinalGuards(t *testing.T) {
	for _, phase := range []string{"initial", "final"} {
		for _, tc := range activityDiagnosticCases() {
			// Exhaustive mapping/privacy checks above reuse one fixture. These
			// native cases prove refusal, unknown fallback and unchanged positive
			// admission at both boundaries without repeated schema setup per code.
			if tc.name != "rate limited" && tc.name != "unknown error" && tc.name != "complete no signal" {
				continue
			}
			t.Run(phase+"/"+tc.name, func(t *testing.T) {
				f := newHostedDriverFixture(t)
				var op reviewexec.Operation
				var attempt reviewexec.Attempt
				var config reviewexec.Configuration
				if phase == "final" {
					op, attempt = f.prepareHosted(t)
				} else {
					op, attempt, config = f.startHosted(t)
				}
				f.hosted.provider = &classifiedActivityProvider{f.provider, tc.observation, tc.failure}
				if phase == "initial" {
					prepared, err := f.hosted.Prepare(t.Context(), op, config, attempt)
					if tc.want == "" {
						if err != nil || prepared.Intent == "" || f.provider.queries != 1 || f.provider.reviews != 0 {
							t.Fatal("idle preparation was refused", err)
						}
						return
					}
					var refusal *core.Error
					if !errors.As(err, &refusal) || refusal.Details.(map[string]string)["reason"] != tc.want || prepared.Intent != "" || f.provider.queries != 0 || f.provider.reviews != 0 || hostedDriverJournal(t, f, attempt.ID) != "preparing" {
						t.Fatal("initial activity gate posted or lost original cause", err)
					}
				} else {
					result, err := f.hosted.Dispatch(t.Context(), op, attempt, f.callback(t, &attempt))
					if tc.want == "" {
						if err != nil || result.State != "completed" || f.provider.reviews != 1 || f.provider.queries != 1 {
							t.Fatal("idle final guard changed permission", result, err)
						}
						return
					}
					if err != nil || result.State != "failed" || result.Reason != tc.want || f.provider.reviews != 0 || f.provider.queries != 1 || hostedDriverJournal(t, f, attempt.ID) != "posting" {
						t.Fatal("final activity gate posted, advanced or discarded cause", result, err)
					}
				}
				if cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
					t.Fatal("activity refusal created review credit")
				}
			})
		}
	}
}
