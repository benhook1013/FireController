package reviewexec

import (
	"context"
	"errors"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestCIWatchHistoricalReaderReturnsGateCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := &ciWatchHistoricalReader{ciWatchReader: &ciWatchReader{block: make(chan struct{})}}
	observation, rows, err := r.ObserveCIGateWithWorkflowAttention(ctx, reviewprovider.CITarget{PullNumber: 42}, "main", []int64{10}, []int64{10}, []int64{10})
	if !errors.Is(err, context.Canceled) || len(rows) != 0 || observation.Actions != nil || r.historical.Load() != 0 {
		t.Fatal("canceled gate produced historical proof or lost its error", observation, rows, err)
	}
}
