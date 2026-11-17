package reviewexec

import (
	"context"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"sync/atomic"
	"testing"
	"time"
)

func TestAutomaticSelectionDoesNotReadQueuedSuffix(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2, 3, 4, 5})
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	r := schedulingLatest(t, f)
	f.enqueue(t, attemptProject, "accepted-later", "hosted", 3, 1)
	c, err := f.e.configurationRevision(t.Context(), r.ProjectID, r.ConfigurationRevision)
	if err != nil {
		t.Fatal(err)
	}
	var suffix atomic.Int32
	reader := &refreshTestReader{pull: func(ctx context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		if n >= 3 {
			suffix.Add(1)
		}
		p := schedulingPull(n)
		isDraft := true
		p.IsDraft = &isDraft
		return p, nil
	}}
	if _, err = f.e.chooseRunWithSourceBudget(t.Context(), r, 801, c, reader, time.Second); err != nil {
		t.Fatal(err)
	}
	if suffix.Load() != 0 {
		t.Fatal("read ineligible queued suffix", suffix.Load())
	}
}
