package reviewexec

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

type lifecycleHostedOnlyReader struct{ ExternalHostedReader }

func TestLifecycleMergeStateNormalization(t *testing.T) {
	for _, state := range []string{"BEHIND", "BLOCKED", "CLEAN", "DIRTY", "DRAFT", "HAS_HOOKS", "UNKNOWN", "UNSTABLE", "", "FUTURE_STATE", "clean", " private-provider-value "} {
		for _, lane := range []lifecycleLane{lifecycleMetadata, lifecycleEnrichment} {
			t.Run(fmt.Sprintf("%s/lane-%d", state, lane), func(t *testing.T) {
				f, s, clock := lifecycleFixture(t)
				currentState := "CLEAN"
				r := &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) {
					p := lifecyclePull(2)
					p.MergeStateStatus = currentState
					return p, nil
				}}}
				readers := map[string]TargetReader{"test-capability": r}
				baseline, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane)
				if err != nil || baseline == nil {
					t.Fatal(baseline, err)
				}
				if err = f.e.observeLifecycle(t.Context(), *baseline, s); err != nil {
					t.Fatal(err)
				}
				clock.Add(time.Minute)
				currentState = state
				target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane)
				if err != nil || target == nil {
					t.Fatal(target, err)
				}
				if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil {
					t.Fatalf("provider merge state stopped lifecycle: %v", err)
				}
				var got string
				err = f.store.DB().QueryRow(`SELECT json_extract(v.metadata_json,'$.merge_state_status') FROM review_current_pulls c JOIN review_pull_memberships m ON m.project_id=c.project_id AND m.number=c.number AND m.start_revision=c.start_revision JOIN review_pull_versions v ON v.project_id=m.project_id AND v.number=m.number AND v.version_revision=m.version_revision WHERE c.project_id=? AND c.number=2`, attemptProject).Scan(&got)
				if err != nil {
					t.Fatal(err)
				}
				want := state
				switch state {
				case "", "FUTURE_STATE", "clean", " private-provider-value ":
					want = "UNKNOWN"
				}
				if got != want {
					t.Fatalf("merge state=%q want=%q", got, want)
				}
				for _, account := range s.accounts {
					if account.backoff != 0 || !account.until.IsZero() {
						t.Fatal("local normalization cooled account")
					}
				}
				clock.Add(time.Minute)
				again, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane)
				if err != nil || again == nil {
					t.Fatal("normal polling stopped", again, err)
				}
				if err = f.e.observeLifecycle(t.Context(), *again, s); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestLifecycleCIObservationDiagnostics(t *testing.T) {
	for _, mode := range []string{"wrong-target", "invalid-state", "zero-start", "stale-start", "reversed-time", "future-time", "missing-reader", "provider-error", "provider-unauthorized", "provider-timeout", "satisfied", "unproven", "missing-required-check", "mixed-required-reasons"} {
		t.Run(mode, func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			attachLifecycleHints(t, f, s)
			base := refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { return lifecyclePull(2), nil }}
			r := &sourceCITestReader{refreshTestReader: refreshTestReader{pull: base.pull}, ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				o := sourceCITestObservation(target, "satisfied")
				switch mode {
				case "wrong-target":
					o.Target.PullNumber++
				case "invalid-state":
					o.State = "unsupported"
				case "zero-start":
					o.StartedAt = time.Time{}
				case "stale-start":
					o.StartedAt = o.StartedAt.Add(-time.Hour)
				case "reversed-time":
					o.ObservedAt = o.StartedAt.Add(-time.Second)
				case "future-time":
					o.ObservedAt = o.ObservedAt.Add(time.Minute)
				case "provider-error":
					return reviewprovider.CIObservation{}, &reviewprovider.Error{Code: "provider_unavailable"}
				case "provider-unauthorized":
					return reviewprovider.CIObservation{}, &reviewprovider.Error{Code: "provider_unauthorized"}
				case "provider-timeout":
					return reviewprovider.CIObservation{}, &reviewprovider.Error{Code: "provider_unavailable", Status: 408}
				case "unproven":
					o = reviewprovider.CIObservation{Target: target, StartedAt: o.StartedAt, ObservedAt: o.ObservedAt, State: "unproven", Reason: "ci_required_checks_unproven"}
				case "missing-required-check", "mixed-required-reasons":
					o.Authority.Checks = append(o.Authority.Checks, reviewprovider.RequiredCheck{Context: "validate", AppID: 1})
					if mode == "mixed-required-reasons" {
						// The build check lacks workflow-origin proof, while validate
						// is absent. Neither reason alone describes the required gate.
						o.Results.Checks = append(o.Results.Checks, reviewprovider.CICheckRun{ID: 992, SuiteID: 993, AppID: 1, Name: "build", HeadSHA: target.HeadSHA, Status: "completed", Conclusion: "success", StartedAt: o.StartedAt, CompletedAt: o.ObservedAt})
					}
					var err error
					o.Assessment, err = reviewprovider.AssessRequiredCI(o.Authority, *o.Results, *o.Actions, o.Attempts)
					if err != nil {
						t.Fatal(err)
					}
					o.State, o.Reason = o.Assessment.State, o.Assessment.Reason
				}
				return o, nil
			}}
			var reader TargetReader = r
			if mode == "missing-reader" {
				reader = &lifecycleHostedOnlyReader{ExternalHostedReader: externalReader(reviewprovider.HostedPublicationSnapshot{Complete: true})}
			}
			target, err := f.e.claimLifecycleLane(t.Context(), map[string]TargetReader{"test-capability": reader}, s, lifecycleEnrichment)
			if err != nil || target == nil {
				t.Fatal(target, err)
			}
			if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil {
				t.Fatal(err)
			}
			wantReason, wantState := "ci_observation_unproven", "unknown"
			switch mode {
			case "missing-reader":
				wantReason = "ci_gate_reader_unavailable"
			case "provider-error":
				wantReason = "ci_provider_unavailable"
			case "provider-unauthorized":
				wantReason = "ci_provider_unauthorized"
			case "provider-timeout":
				wantReason = "ci_provider_timeout"
			case "satisfied":
				wantReason, wantState = "", "success"
			case "unproven", "mixed-required-reasons":
				wantReason = "ci_required_checks_unproven"
			case "missing-required-check":
				wantReason = "ci_required_check_missing"
			}
			due := s.due[lifecycleKey{attemptProject, 2}]
			if due.ciError != wantReason || due.ciCheckedAt != clock.Now() {
				t.Fatalf("CI health: reason=%q checked=%v, want %q/%v", due.ciError, due.ciCheckedAt, wantReason, clock.Now())
			}
			got := lifecycleRead(t, f)
			if got.CIState != wantState {
				t.Fatalf("CI state=%s want=%s", got.CIState, wantState)
			}
			_, health, err := f.e.ObservationHealth(t.Context(), attemptProject, []reviews.PullSummary{got})
			if err != nil || health[2].CIError != wantReason || health[2].CICheckedAt != observationTime(clock.Now()) {
				t.Fatal("published CI health lost diagnostic or timestamp", health, err)
			}
			if mode == "missing-required-check" || mode == "mixed-required-reasons" {
				if health[2].CIGateRef != "main" || health[2].CIGateState != "unknown" || due.ciCommit != got.HeadSHA || due.ciHead != got.HeadSHA || due.ciBaseSHA != got.BaseSHA || due.ciBaseRef != got.BaseRef {
					t.Fatal("diagnostic changed gate state or comparison pin", health, due)
				}
			}
			for _, account := range s.accounts {
				if mode == "provider-error" || mode == "provider-unauthorized" || mode == "provider-timeout" {
					if account.backoff <= 0 {
						t.Fatal("lost genuine provider backoff")
					}
				} else if account.backoff != 0 || !account.until.IsZero() {
					t.Fatal("local diagnostic cooled shared account")
				}
			}
		})
	}
}
