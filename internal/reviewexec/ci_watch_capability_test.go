package reviewexec

import (
	"context"
	"errors"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

type optionalCIGateReader struct {
	*ciWatchReader
	available bool
}

func (r *optionalCIGateReader) SupportsCIGate() bool { return r.available }

func TestCIWatchRejectsUnavailableOptionalGateBeforeRegistration(t *testing.T) {
	for _, available := range []bool{false, true} {
		options, _ := lifecycleOptions(nil)
		s := newLifecycleSchedule(options)
		reader := &optionalCIGateReader{ciWatchReader: &ciWatchReader{}, available: available}
		s.readers = map[string]TargetReader{"optional": reader}
		sub, err := s.registerCIWatch(core.Scope{PrincipalID: "principal-one"}, ciWatchBinding{capability: "optional"}, []CIWatchTarget{{PullNumber: 2}})
		if !available {
			var failure *core.Error
			if sub != nil || !errors.As(err, &failure) || failure.Code != "overloaded" || ciWatchDiagnosticReason(t, err) != "ci_watch_observer_unavailable" || len(s.watchers) != 0 || len(s.watches) != 0 || len(s.watchPrincipals) != 0 || reader.calls != 0 {
				t.Fatal("unsupported optional observer consumed watch capacity or provider work", sub, err)
			}
		} else {
			if err != nil || sub == nil || len(s.watchers) != 1 {
				t.Fatal(sub, err)
			}
			s.unregisterCIWatch(sub)
		}
	}
}

// This adapter exposes the attention method even when its wrapped provider does not.
type optionalCIWorkflowAttentionReader struct {
	*ciWatchWorkflowReader
	available bool
}

func (r *optionalCIWorkflowAttentionReader) SupportsCIWorkflowAttention() bool { return r.available }
func (r *optionalCIWorkflowAttentionReader) ObserveCIGateWithWorkflowAttention(ctx context.Context, target reviewprovider.CITarget, gate string, ids, heads, historical []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error) {
	return r.ObserveCIGateWithWorkflows(ctx, target, gate, ids)
}

func TestCIWatchRejectsUnsupportedAdvancedAttentionBeforeRegistration(t *testing.T) {
	for _, mode := range []string{"head", "historical", "both", "ordinary"} {
		for _, kind := range []string{"legacy", "unsupported-adapter", "supported-adapter"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				options, _ := lifecycleOptions(nil)
				s := newLifecycleSchedule(options)
				base := &ciWatchReader{}
				legacy := &ciWatchWorkflowReader{ciWatchReader: base}
				var reader TargetReader = legacy
				if kind != "legacy" {
					reader = &optionalCIWorkflowAttentionReader{ciWatchWorkflowReader: legacy, available: kind == "supported-adapter"}
				}
				s.readers = map[string]TargetReader{"optional": reader}
				target := CIWatchTarget{PullNumber: 2, FailureWorkflowIDs: []int64{10}, AllowHeadAttention: mode == "head" || mode == "both", AllowHistoricalAttention: mode == "historical" || mode == "both"}
				// A later unsupported target must not partially register the first.
				sub, err := s.registerCIWatch(core.Scope{PrincipalID: "principal"}, ciWatchBinding{capability: "optional"}, []CIWatchTarget{{PullNumber: 1}, target})
				if mode != "ordinary" && kind != "supported-adapter" {
					var failure *core.Error
					if sub != nil || !errors.As(err, &failure) || failure.Code != "ci_watch_workflow_failures_unsupported" || len(s.watchers) != 0 || len(s.watches) != 0 || len(s.watchPrincipals) != 0 || base.calls != 0 {
						t.Fatal("unsupported mode consumed work or capacity", sub, err)
					}
				} else {
					if err != nil || sub == nil || len(s.watchers) != 1 {
						t.Fatal(sub, err)
					}
					s.unregisterCIWatch(sub)
				}
			})
		}
	}
}
