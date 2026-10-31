package reviewexec

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestLifecycleCIFailureRetainsSafeStageAndCooldown(t *testing.T) {
	for _, tc := range []struct {
		name, stage, code, reason string
		status                    int
		retry                     int64
		cooldown                  time.Duration
	}{
		{"invalid", "head_results", "provider_invalid_response", "ci_provider_invalid_response_head_results", 200, 0, 0},
		{"timeout", "attempt", "provider_unavailable", "ci_provider_timeout_attempt", 408, 0, 30 * time.Second},
		{"rate", "actions", "provider_rate_limited", "ci_provider_rate_limited_actions", 429, 120, 2 * time.Minute},
		{"unknown_stage", "PRIVATE_PROVIDER_URL", "provider_invalid_response", "ci_provider_invalid_response", 200, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			f.e.lifecycleMu.Lock()
			f.e.lifecycleSchedule = s
			f.e.lifecycleMu.Unlock()
			f.e.lifecycleRunning.Store(true)
			t.Cleanup(func() { f.e.lifecycleRunning.Store(false) })
			r := &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) {
				return lifecyclePull(2), nil
			}}, ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				// An accompanying partial success is never evidence after failure.
				return sourceCITestObservation(target, "satisfied"), sourceCIStageFailure{stage: tc.stage, cause: &reviewprovider.Error{Code: tc.code, Status: tc.status, RetryAfterSeconds: tc.retry, Message: "PRIVATE_PROVIDER_MESSAGE"}}
			}}
			lifecycleTick(t, f, s, r)
			pull := lifecycleRead(t, f)
			if pull.CIState != "unknown" || pull.Mergeability != "mergeable" || r.ciCalls.Load() != 1 {
				t.Fatal("failed CI adopted partial success or lost source facts", pull, r.ciCalls.Load())
			}
			_, health, err := f.e.ObservationHealth(t.Context(), attemptProject, []reviews.PullSummary{pull})
			if err != nil || health[2].CIError != tc.reason || health[2].CICheckedAt != observationTime(clock.Now()) || health[2].LastSuccessAt != observationTime(clock.Now()) {
				t.Fatal("safe CI stage missing from automatic observation", health, err)
			}
			body, err := json.Marshal(health)
			if err != nil || strings.Contains(string(body), "PRIVATE") {
				t.Fatal("private diagnostic leaked", string(body), err)
			}
			for _, account := range s.accounts {
				if tc.cooldown == 0 {
					if !account.until.IsZero() || account.lastErrorCode != "" {
						t.Fatal("invalid inventory blocked the account", account)
					}
				} else if account.until != clock.Now().Add(tc.cooldown) || account.lastErrorCode != tc.code {
					t.Fatal("CI diagnostic changed shared cooldown", account)
				}
			}
			before := attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`)
			for range 2 {
				if _, _, err := f.e.ObservationHealth(t.Context(), attemptProject, []reviews.PullSummary{pull}); err != nil {
					t.Fatal(err)
				}
			}
			if r.ciCalls.Load() != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`) != before {
				t.Fatal("diagnostic read recollected or changed retained facts")
			}
			for _, table := range []string{"review_operations", "review_execution_attempts", "review_source_refreshes"} {
				if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
					t.Fatal("diagnostic created an action", table)
				}
			}
		})
	}
}
