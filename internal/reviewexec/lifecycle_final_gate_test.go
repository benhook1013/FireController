package reviewexec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

type lifecycleFinalGateReader struct {
	refreshTestReader
	gate  func(context.Context, reviewprovider.CITarget, string) (reviewprovider.CIObservation, error)
	calls int
}

func (r *lifecycleFinalGateReader) ObserveCIGate(ctx context.Context, target reviewprovider.CITarget, gate string) (reviewprovider.CIObservation, error) {
	r.calls++
	return r.gate(ctx, target, gate)
}
func TestLifecycleFinalGateStackedResults(t *testing.T) {
	for _, state := range []string{"satisfied", "pending", "failed", "not_required", "provider_failure", "wrong_gate", "old_observation"} {
		t.Run(state, func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			attachLifecycleHints(t, f, s)
			p := lifecyclePull(2)
			p.BaseRef = "feature/parent"
			r := &lifecycleFinalGateReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { return p, nil }}}
			r.gate = func(_ context.Context, target reviewprovider.CITarget, gate string) (reviewprovider.CIObservation, error) {
				if gate != "main" || target.BaseRef != p.BaseRef || target.BaseSHA != p.BaseSHA || target.HeadSHA != p.HeadSHA {
					t.Fatal("changed actual comparison or wrong final gate", target, gate)
				}
				result := state
				if state == "provider_failure" || state == "wrong_gate" || state == "old_observation" {
					result = "satisfied"
				}
				o := sourceCITestObservation(target, result)
				o.Authority.Branch = gate
				if state == "provider_failure" {
					return o, errors.New("private partial success")
				}
				if state == "wrong_gate" {
					o.Authority.Branch = target.BaseRef
				}
				if state == "old_observation" {
					o.StartedAt = o.StartedAt.Add(-time.Hour)
				}
				return o, nil
			}
			lifecycleTick(t, f, s, r)
			got := lifecycleRead(t, f)
			want := sourceCIQueueState(state)
			if got.CIState != want || got.BaseRef != p.BaseRef || r.calls != 1 {
				t.Fatal("wrong stacked result", got, r.calls, want)
			}
			_, health, err := f.e.ObservationHealth(t.Context(), attemptProject, []reviews.PullSummary{got})
			if err != nil {
				t.Fatal(err)
			}
			if want != "unknown" && (health[2].CIGateRef != "main" || health[2].CIGateState != want) {
				t.Fatal("missing final gate", health)
			}
			if want == "unknown" && (health[2].CIGateRef != "" || health[2].CIGateState != "") {
				t.Fatal("unvalidated gate credited", health)
			}
			clock.Add(time.Minute)
			lifecycleTick(t, f, s, r)
			if r.calls != 1 {
				t.Fatal("cheap metadata poll repeated CI", r.calls)
			}
			got.HeadSHA = strings.Repeat("f", 40)
			_, health, err = f.e.ObservationHealth(t.Context(), attemptProject, []reviews.PullSummary{got})
			if err != nil || (health[2].CIGateRef != "" || health[2].CIGateState != "") {
				t.Fatal("moved head inherited final gate", health, err)
			}
		})
	}
}
func TestLifecycleFinalGateRefusals(t *testing.T) {
	for _, mode := range []string{"missing_default", "changed_default", "repository_failure", "changed_identity", "missing_gate_reader", "actual_base_drift"} {
		t.Run(mode, func(t *testing.T) {
			p := lifecyclePull(2)
			p.BaseRef = "feature/parent"
			if mode == "actual_base_drift" {
				p.BaseRef = "main"
			}
			repo := reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}
			if mode == "missing_default" {
				repo.DefaultBranch = ""
			}
			plan := SourcePlan{Repository: repo, Pull: p, ObservedAt: stamp()}
			r := &lifecycleFinalGateReader{}
			r.repository = func(context.Context, int64) (reviewprovider.Repository, error) {
				v := repo
				switch mode {
				case "changed_default":
					v.DefaultBranch = "develop"
				case "repository_failure":
					return v, errors.New("private error")
				case "changed_identity":
					v.ID++
				}
				return v, nil
			}
			r.gate = func(_ context.Context, target reviewprovider.CITarget, gate string) (reviewprovider.CIObservation, error) {
				o := sourceCITestObservation(target, "satisfied")
				o.Authority.Branch = gate
				if mode == "actual_base_drift" {
					o.Target.BaseSHA = strings.Repeat("d", 40)
				}
				return o, nil
			}
			var reader TargetReader = r
			if mode == "missing_gate_reader" {
				reader = &sourceCITestReader{}
			}
			o, reason, err := observeLifecycleCIGate(t.Context(), reader, plan)
			if reason == "" && err == nil || o.State == "satisfied" || o.State == "not_required" {
				t.Fatal("unproven final gate credited", o, reason, err)
			}
		})
	}
}
