package reviewexec

import (
	"context"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"strings"
	"testing"
	"time"
)

func TestIdleWorkerProbesDraftWhilePeerOwnsHostedWork(t *testing.T) {
	for _, stage := range []string{"prepare", "dispatch"} {
		t.Run(stage, func(t *testing.T) {
			f := supervisorFixture(t)
			f.enqueue(t, attemptProject, "long-hosted", "hosted", 2, 1)
			entered := make(chan struct{})
			release := make(chan struct{})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			block := func(ctx context.Context) error {
				close(entered)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			d := &draftWaitDriver{reader: &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
				ready := false
				return reviewprovider.Pull{Number: n, RepositoryID: 801, State: "open", IsDraft: &ready, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
			}}}
			d.prepare = func(ctx context.Context, o Operation, _ Configuration, a Attempt) (Preparation, error) {
				if o.Channel == "hosted" && stage == "prepare" {
					if err := block(ctx); err != nil {
						return Preparation{}, err
					}
				}
				p := Preparation{Anchor: attemptAnchor(o.ProjectID, o.PullNumber)}
				if o.Channel == "hosted" {
					p.Intent = "synthetic-intent-" + a.ID
				}
				return p, nil
			}
			d.dispatch = func(ctx context.Context, o Operation, _ Attempt, _ func(context.Context, string) error) (Result, error) {
				if o.Channel == "hosted" && stage == "dispatch" {
					if err := block(ctx); err != nil {
						return Result{}, err
					}
				}
				return Result{State: "completed", Reason: "synthetic_capture"}, nil
			}
			drivers := map[string]Driver{"test-capability": d}
			done := make(chan error, 1)
			go func() { done <- f.e.processNext(ctx, drivers) }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("peer did not start Hosted work")
			}
			if f.e.draftProbePermit.Load() {
				t.Fatal("long Hosted work retained probe permit")
			}
			draft := f.enqueue(t, attemptProject, "ready-draft", "cli", 2, 1)
			markCLIDraftWait(t, f, draft.ID)
			if err := f.e.processNext(ctx, drivers); err != nil {
				t.Fatal(err)
			}
			o := cliWaitRead(t, f, draft.ID)
			if o.State != "completed" {
				t.Fatal("idle worker failed ready Draft while Hosted peer blocked", o.State)
			}
			if !draftProbeClaimed(f.e, draft.ID) {
				t.Fatal("Draft resumed without bounded source probe")
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if f.e.draftProbePermit.Load() {
				t.Fatal("probe permit leaked")
			}
		})
	}
}
