package reviewprovider

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestWorkflowAttentionCancellationAfterGatePreservesFacts(t *testing.T) {
	f := ciWorkflowFailureFixture(t, "failure")
	target := ciObservationTarget()
	gate, err := f.g.ObserveCIGate(context.Background(), target, "main")
	if err != nil || gate.State != "pending" {
		t.Fatal("gate fixture failed", gate, err)
	}
	before, _ := f.snapshot()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, rows, err := f.g.collectCIWorkflowAttention(ctx, target, gate, map[int64]bool{31: true}, nil, nil)
	var warning *CIWorkflowAttentionError
	if !errors.As(err, &warning) || !errors.Is(err, context.Canceled) || len(rows) != 0 {
		t.Fatal("optional cancellation was reported as empty successful collection", rows, err)
	}
	if !reflect.DeepEqual(got, gate) {
		t.Fatal("optional cancellation changed completed gate facts", got, gate)
	}
	after, _ := f.snapshot()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("canceled collection started provider reads", before, after)
	}
	// A canceled context does not turn an unselected workflow into collection.
	got, rows, err = f.g.collectCIWorkflowAttention(ctx, target, gate, map[int64]bool{999: true}, nil, nil)
	if err != nil || len(rows) != 0 || !reflect.DeepEqual(got, gate) {
		t.Fatal("unselected workflow was treated as canceled collection", rows, err)
	}
}
