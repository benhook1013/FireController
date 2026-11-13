package reviewexec

import (
	"context"
	"errors"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type advertisedLifecycleGateReader struct {
	*lifecycleFinalGateReader
	available bool
}

func (r *advertisedLifecycleGateReader) SupportsCIGate() bool { return r.available }

type advertisedLifecycleBaseReader struct {
	*advertisedLifecycleGateReader
	baseCalls int
}

func (r *advertisedLifecycleBaseReader) ObserveRequiredCI(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
	r.baseCalls++
	return sourceCITestObservation(target, "satisfied"), nil
}

func TestLifecycleGateCapabilitySelection(t *testing.T) {
	for _, mode := range []string{"unsupported", "base_same", "base_distinct", "advertised_gate", "legacy_gate", "provider_error"} {
		t.Run(mode, func(t *testing.T) {
			p := lifecyclePull(2)
			p.BaseRef = "main"
			if mode == "base_distinct" {
				p.BaseRef = "feature/parent"
			}
			repo := reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}
			plan := SourcePlan{Repository: repo, Pull: p, ObservedAt: stamp()}
			gate := &lifecycleFinalGateReader{}
			gate.repository = func(context.Context, int64) (reviewprovider.Repository, error) { return repo, nil }
			providerErr := errors.New("actual provider failure")
			gate.gate = func(_ context.Context, target reviewprovider.CITarget, branch string) (reviewprovider.CIObservation, error) {
				if mode == "provider_error" {
					return reviewprovider.CIObservation{}, providerErr
				}
				o := sourceCITestObservation(target, "satisfied")
				o.Authority.Branch = branch
				return o, nil
			}
			advertised := &advertisedLifecycleGateReader{lifecycleFinalGateReader: gate, available: mode == "advertised_gate" || mode == "provider_error"}
			base := &advertisedLifecycleBaseReader{advertisedLifecycleGateReader: advertised}
			var reader TargetReader = advertised
			if mode == "legacy_gate" {
				reader = gate
			}
			if mode == "base_same" || mode == "base_distinct" {
				reader = base
			}
			o, reason, err := observeLifecycleCIGate(t.Context(), reader, plan)
			switch mode {
			case "unsupported", "base_distinct":
				if err != nil || reason != "ci_gate_reader_unavailable" || o.State != "" || gate.calls != 0 || base.baseCalls != 0 {
					t.Fatal("unsupported capability fabricated outage or crossed base", o, reason, err, gate.calls, base.baseCalls)
				}
			case "base_same":
				if err != nil || reason != "" || o.State != "satisfied" || gate.calls != 0 || base.baseCalls != 1 {
					t.Fatal("safe legacy base fallback lost", o, reason, err, gate.calls, base.baseCalls)
				}
			case "provider_error":
				if !errors.Is(err, providerErr) || gate.calls != 1 || base.baseCalls != 0 {
					t.Fatal("real provider outage hidden", err, gate.calls)
				}
			default:
				if err != nil || reason != "" || o.State != "satisfied" || gate.calls != 1 || base.baseCalls != 0 {
					t.Fatal("supported gate lost", o, reason, err, gate.calls)
				}
			}
		})
	}
}
