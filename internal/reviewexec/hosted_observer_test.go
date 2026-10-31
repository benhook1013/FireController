package reviewexec

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func observerFixture(t *testing.T) (*Engine, Operation) {
	t.Helper()
	e, err := New(nil, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	return e, Operation{ID: "review_operation_" + strings.Repeat("a", 32), ProjectID: attemptProject, AttemptID: "review_attempt_" + strings.Repeat("b", 32), Channel: "hosted", State: "uncertain", CapabilityFingerprint: e.capabilities["test-capability"].Fingerprint}
}

func TestHostedObserverConcurrentChecksBindingAndRestart(t *testing.T) {
	e, o := observerFixture(t)
	unknown := &HostedObserverStatus{State: "unknown"}
	if got := e.observerStatus(o); !reflect.DeepEqual(got, unknown) {
		t.Fatal(got)
	}
	first := e.BeginHostedObserver("test-capability", o.ProjectID, o.ID, o.AttemptID)
	second := e.BeginHostedObserver("test-capability", o.ProjectID, o.ID, o.AttemptID)
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	e.FinishHostedObserver(first, at, at, "attribution_window_closed", nil)
	if got := e.observerStatus(o); got.State != "checking" || got.LastSourceAt == "" || got.Reason != "attribution_window_closed" {
		t.Fatal(got)
	}
	e.StopHostedObserver(first, nil)
	if got := e.observerStatus(o); got.State != "checking" {
		t.Fatal("worker stop hid overlapping pass", got)
	}
	e.FinishHostedObserver(second, at.Add(-time.Second), at.Add(-time.Second), "publication_not_proven", nil)
	if got := e.observerStatus(o); got.State != "stopped" || got.Error != "" || got.LastSourceAt != at.Add(-time.Second).Format(time.RFC3339Nano) {
		t.Fatal("clock correction or stop misreported", got)
	}
	current := e.BeginHostedObserver("test-capability", o.ProjectID, o.ID, o.AttemptID)
	e.FinishHostedObserver(current, at, time.Time{}, "source_unavailable", errors.New("private provider body"))
	e.ScheduleHostedObserver(current, at.Add(time.Minute))
	got := e.observerStatus(o)
	if got.State != "retrying" || got.Error != "observer_unavailable" || got.LastSourceAt == "" || got.NextCheckAt == "" {
		t.Fatal(got)
	}
	got.Error = "caller mutation"
	if e.observerStatus(o).Error != "observer_unavailable" {
		t.Fatal("shared status pointer")
	}
	for name, change := range map[string]func(*Operation){
		"project":     func(o *Operation) { o.ProjectID = otherAttemptProject },
		"operation":   func(o *Operation) { o.ID = "review_operation_" + strings.Repeat("c", 32) },
		"attempt":     func(o *Operation) { o.AttemptID = "review_attempt_" + strings.Repeat("d", 32) },
		"fingerprint": func(o *Operation) { o.CapabilityFingerprint = strings.Repeat("f", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			other := o
			change(&other)
			if got := e.observerStatus(other); !reflect.DeepEqual(got, unknown) {
				t.Fatal("cross-binding leak", got)
			}
		})
	}
	for _, state := range []string{"queued", "preparing", "held", "completed", "cancelled", "failed", "rate_limited"} {
		other := o
		other.State = state
		if e.observerStatus(other) != nil {
			t.Fatal("nonpending state", state)
		}
	}
	other := o
	other.Channel = "cli"
	if e.observerStatus(other) != nil {
		t.Fatal("CLI observer")
	}
	other = o
	other.AttemptID = ""
	if e.observerStatus(other) != nil {
		t.Fatal("missing current attempt")
	}
	restarted, _ := observerFixture(t)
	if got := restarted.observerStatus(o); !reflect.DeepEqual(got, unknown) {
		t.Fatal("restart fabricated health", got)
	}
	// An old pass may finish after a new operation has replaced the bounded slot.
	old := e.BeginHostedObserver("test-capability", o.ProjectID, o.ID, o.AttemptID)
	other = o
	other.ID = "review_operation_" + strings.Repeat("c", 32)
	other.AttemptID = "review_attempt_" + strings.Repeat("d", 32)
	newer := e.BeginHostedObserver("test-capability", other.ProjectID, other.ID, other.AttemptID)
	e.FinishHostedObserver(old, at, at, "source_unavailable", errors.New("private"))
	e.ScheduleHostedObserver(old, at.Add(time.Hour))
	e.StopHostedObserver(old, errors.New("private"))
	if got := e.observerStatus(other); got.State != "checking" || got.LastCheckAt != "" || got.Error != "" || got.NextCheckAt != "" {
		t.Fatal("late old pass corrupted replacement", got)
	}
	e.FinishHostedObserver(newer, at, at, "publication_not_proven", nil)
	if len(e.hostedObservers) != 1 {
		t.Fatal("unbounded attempt history")
	}
	if e.BeginHostedObserver("unknown-capability", o.ProjectID, o.ID, o.AttemptID) != nil {
		t.Fatal("unknown capability allocated state")
	}
}

func TestHostedObserverSafeCodesAndRace(t *testing.T) {
	for _, reason := range []string{"attribution_window_closed", "publication_not_proven", "hosted_observation_capacity", "observation_incomplete"} {
		if hostedObserverReason(reason) != reason {
			t.Fatal(reason)
		}
	}
	if hostedObserverReason("private-secret") != "observation_unproven" {
		t.Fatal("arbitrary reason leaked")
	}
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""}, {errors.New("private provider body"), "observer_unavailable"},
		{&reviewprovider.Error{Code: "private-secret"}, "observer_unavailable"},
		{core.Fail("private-secret", "private body", 400), "observer_unavailable"},
		{core.Fail("overloaded", "private body", 503), "observer_overloaded"},
		{fmt.Errorf("private: %w", context.DeadlineExceeded), "provider_unavailable"},
		{context.Canceled, "observer_cancelled"},
	}
	for _, code := range []string{"provider_rate_limited", "provider_unauthorized", "provider_not_found", "provider_invalid_response", "provider_unavailable"} {
		cases = append(cases, struct {
			err  error
			want string
		}{&reviewprovider.Error{Code: code}, code})
	}
	for _, tc := range cases {
		if got := hostedObserverError(tc.err); got != tc.want {
			t.Fatal(got, tc.want)
		}
	}
	e, o := observerFixture(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				check := e.BeginHostedObserver("test-capability", o.ProjectID, o.ID, o.AttemptID)
				at := time.Now().UTC()
				e.FinishHostedObserver(check, at, at, "publication_not_proven", nil)
				e.ScheduleHostedObserver(check, at.Add(time.Minute))
				_ = e.observerStatus(o)
			}
		}()
	}
	wg.Wait()
	if got := e.observerStatus(o); got.State != "waiting" || got.Error != "" || got.LastCheckAt == "" || got.LastSourceAt == "" {
		t.Fatal(got)
	}
}
