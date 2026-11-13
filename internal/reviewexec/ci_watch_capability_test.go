package reviewexec

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
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
	available         bool
	heads, historical []int64
}

func (r *optionalCIWorkflowAttentionReader) SupportsCIWorkflowAttention() bool { return r.available }
func (r *optionalCIWorkflowAttentionReader) ObserveCIGateWithWorkflowAttention(ctx context.Context, target reviewprovider.CITarget, gate string, ids, heads, historical []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error) {
	r.heads, r.historical = slices.Clone(heads), slices.Clone(historical)
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

func TestCIWatchCollectsSupportedAdvancedAttention(t *testing.T) {
	for _, test := range []struct {
		name              string
		heads, historical []int64
	}{
		{name: "head", heads: []int64{10}},
		{name: "historical", historical: []int64{10}},
		{name: "both", heads: []int64{10}, historical: []int64{10}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, schedule, _, base, scope := ciWatchFixture(t)
			adapter := &optionalCIWorkflowAttentionReader{ciWatchWorkflowReader: &ciWatchWorkflowReader{ciWatchReader: base}, available: true}
			schedule.readers["test-capability"] = adapter
			target := CIWatchTarget{
				PullNumber: 42, HeadSHA: strings.Repeat("a", 40), FailureWorkflowIDs: []int64{10},
				AllowHeadAttention: len(test.heads) > 0, AllowHistoricalAttention: len(test.historical) > 0,
			}
			_, cancel := startCIWatch(t, f, scope, []CIWatchTarget{target})
			defer cancel()
			ciWatchRegistered(t, schedule, 1)
			watchTick(t, f, schedule)
			if !slices.Equal(adapter.heads, test.heads) || !slices.Equal(adapter.historical, test.historical) {
				t.Fatalf("provider selectors heads=%v historical=%v, want %v / %v", adapter.heads, adapter.historical, test.heads, test.historical)
			}
		})
	}
}
