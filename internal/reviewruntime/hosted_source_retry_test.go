package reviewruntime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type limitedHostedSource struct {
	*driverHostedProvider
	observed time.Time
}

func (p *limitedHostedSource) Pull(context.Context, string, int64) (reviewprovider.Pull, error) {
	return reviewprovider.Pull{}, &reviewprovider.Error{Code: "provider_rate_limited", Status: 429, RetryTiming: reviewprovider.RetryTimingEvidence{State: "unknown", ObservedAt: p.observed}}
}

func TestHostedFirstSourceRateLimitRetainsOriginalUnusedRequest(t *testing.T) {
	f := newHostedDriverFixture(t)
	engine, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: f.capability.ID, Provider: "coderabbit", Account: f.capability.Account, Channels: f.capability.Channels, Fingerprint: f.capability.Fingerprint}}, reviewexec.WithNativeProjection(), reviewexec.WithSourceReadGroups(map[string]string{f.capability.ID: strings.Repeat("a", 64)}))
	if err != nil {
		t.Fatal(err)
	}
	f.engine, f.hosted.engine = engine, engine
	op, a, c := f.startHosted(t)
	f.hosted.provider = &limitedHostedSource{f.provider, time.Now().UTC()}
	_, failure := f.hosted.Prepare(t.Context(), op, c, a)
	if ok, err := engine.ScheduleSourceRetry(t.Context(), f.project, a.ID, a.Revision, failure); err != nil || !ok {
		t.Fatal("typed initial refusal lost", failure, ok, err)
	}
	retained, err := engine.ReadOperation(t.Context(), f.scope(t, f.writer, false), op.ID)
	if err != nil || retained.State != "queued" || retained.Wait == nil || retained.Wait.SourceRetry == nil || retained.Wait.BaselineAttemptID != "" || f.provider.queries != 0 || f.provider.reviews != 0 {
		t.Fatal(retained, err)
	}
	if _, err := engine.StartAttempt(t.Context(), f.project, op.ID); err != nil {
		t.Fatal(err)
	}
	if f.provider.queries != 0 || f.provider.reviews != 0 {
		t.Fatal("source retry posted provider command")
	}
}
