package reviewruntime

import (
	"context"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type externalWorkflowProvider struct {
	*driverHostedProvider
	ordinary int
}

func (p *externalWorkflowProvider) ObserveCIGateWithWorkflows(ctx context.Context, target reviewprovider.CITarget, _ string, _ []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error) {
	p.ordinary++
	return reviewprovider.CIObservation{Target: target, State: "unproven"}, nil, ctx.Err()
}

type externalHeadAttentionProvider struct {
	*externalWorkflowProvider
	target                      reviewprovider.CITarget
	gate                        string
	ids, headIDs, historicalIDs []int64
}

func (p *externalHeadAttentionProvider) ObserveCIGateWithWorkflowAttention(ctx context.Context, target reviewprovider.CITarget, gate string, ids, heads, historical []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error) {
	p.target, p.gate, p.ids, p.headIDs = target, gate, append([]int64(nil), ids...), append([]int64(nil), heads...)
	p.historicalIDs = append([]int64(nil), historical...)
	return reviewprovider.CIObservation{Target: target, State: "unproven"}, nil, ctx.Err()
}
func TestExternalHostedSourceForwardsExplicitHeadAttentionWithoutCommands(t *testing.T) {
	f := newHostedDriverFixture(t)
	legacy := &externalWorkflowProvider{driverHostedProvider: f.provider}
	p := &externalHeadAttentionProvider{externalWorkflowProvider: legacy}
	source := &hostedArchiveSource{provider: p}
	target := reviewprovider.CITarget{RepositoryID: 901, HeadSHA: "original-head"}
	got, _, err := source.ObserveCIGateWithWorkflowAttention(t.Context(), target, "main", []int64{10, 20}, []int64{20}, []int64{10})
	if err != nil || got.Target != target || p.target != target || p.gate != "main" || !reflect.DeepEqual(p.ids, []int64{10, 20}) || !reflect.DeepEqual(p.headIDs, []int64{20}) || !reflect.DeepEqual(p.historicalIDs, []int64{10}) || legacy.ordinary != 0 {
		t.Fatal("head-only source selection changed", p, got, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err = source.ObserveCIGateWithWorkflowAttention(ctx, target, "main", []int64{20}, []int64{20}, nil); err == nil {
		t.Fatal("cancellation lost")
	}
	if !source.SupportsCIWorkflowAttention() {
		t.Fatal("advanced provider capability lost")
	}
	source.provider = legacy
	if source.SupportsCIWorkflowAttention() {
		t.Fatal("legacy provider advertised advanced attention")
	}
	for _, selectors := range []struct{ heads, historical []int64 }{{[]int64{20}, nil}, {nil, []int64{20}}, {[]int64{20}, []int64{20}}} {
		if _, _, err = source.ObserveCIGateWithWorkflowAttention(t.Context(), target, "main", []int64{20}, selectors.heads, selectors.historical); err == nil || legacy.ordinary != 0 {
			t.Fatal("legacy provider silently discarded advanced selectors", err)
		}
	}
	if _, _, err = source.ObserveCIGateWithWorkflowAttention(t.Context(), target, "main", []int64{20}, nil, nil); err != nil || legacy.ordinary != 1 {
		t.Fatal("older optional source did not retain ordinary semantics", err)
	}
	if f.provider.queries != 0 || f.provider.reviews != 0 || f.provider.activities != 0 {
		t.Fatal("attention forwarding invoked provider commands")
	}
}
