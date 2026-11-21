package reviewexec

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestObservationHealthBindsCIErrorToComparison(t *testing.T) {
	for _, tc := range []struct {
		name      string
		head      string
		baseSHA   string
		baseRef   string
		gateState string
		current   bool
	}{
		{name: "current failed attempt without gate state", head: "head", baseSHA: "base", baseRef: "main", current: true},
		{name: "current gate", head: "head", baseSHA: "base", baseRef: "main", gateState: "failure", current: true},
		{name: "stale head", head: "old-head", baseSHA: "base", baseRef: "main", gateState: "failure"},
		{name: "stale base SHA", head: "head", baseSHA: "old-base", baseRef: "main", gateState: "failure"},
		{name: "stale base ref", head: "head", baseSHA: "base", baseRef: "old-main", gateState: "failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			f.e.lifecycleSchedule = s
			f.e.lifecycleRunning.Store(true)
			at := clock.Now()
			s.due[lifecycleKey{attemptProject, 2}] = lifecycleDue{
				head: "head", baseSHA: "base", baseRef: "main",
				checkedAt: at, successAt: at, next: at.Add(time.Minute), lastErrorCode: "provider_unavailable",
				ciHead: tc.head, ciBaseSHA: tc.baseSHA, ciBaseRef: tc.baseRef,
				ciGateRef: "main", ciGateState: tc.gateState, ciCheckedAt: at, ciError: "ci_unavailable",
				hostedCheckedAt: at, hostedError: "hosted_unavailable",
			}
			_, health, err := f.e.ObservationHealth(t.Context(), attemptProject, []reviews.PullSummary{{Number: 2, HeadSHA: "head", BaseSHA: "base", BaseRef: "main"}})
			if err != nil {
				t.Fatal(err)
			}
			h := health[2]
			if tc.current {
				if h.CICheckedAt != observationTime(at) || h.CIError != "ci_unavailable" || h.CIGateRef != "main" || h.CIGateState != tc.gateState {
					t.Fatalf("current CI attempt lost: %+v", h)
				}
			} else if h.CICheckedAt != "" || h.CIError != "" || h.CIGateRef != "" || h.CIGateState != "" {
				t.Fatalf("stale CI attempt projected onto current comparison: %+v", h)
			}
			if h.State != "retrying" || h.Error != "provider_unavailable" || h.CheckedAt != observationTime(at) || h.LastSuccessAt != observationTime(at) || h.NextCheckAt != observationTime(at.Add(time.Minute)) || h.HostedCheckedAt != observationTime(at) || h.HostedError != "hosted_unavailable" {
				t.Fatalf("general or Hosted observation health changed: %+v", h)
			}
		})
	}
}

func TestObservationHealthSanitizesProviderCodesForTypedQueue(t *testing.T) {
	for _, lane := range []lifecycleLane{lifecycleMetadata, lifecycleEnrichment} {
		for _, code := range []string{"", "PRIVATE provider response", "provider_rate_limited"} {
			t.Run(fmt.Sprintf("lane%d/%q", lane, code), func(t *testing.T) {
				f, s, clock := lifecycleFixture(t)
				f.e.lifecycleSchedule = s
				f.e.lifecycleRunning.Store(true)
				r := &sourceCITestReader{refreshTestReader: refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
					return reviewprovider.Repository{}, fmt.Errorf("wrapped: %w", &reviewprovider.Error{Code: code, RetryAfterSeconds: 120})
				}}}
				target, err := f.e.claimLifecycleLane(t.Context(), map[string]TargetReader{"test-capability": r}, s, lane)
				if err != nil || target == nil {
					t.Fatal(target, err)
				}
				if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil {
					t.Fatal(err)
				}
				var queue reviews.QueueResponse
				projectionRead(t, f, "/queue", &queue)
				observer, health, err := f.e.ObservationHealth(t.Context(), attemptProject, queue.Items)
				if err != nil {
					t.Fatal(err)
				}
				queue.Observer = &observer
				for i := range queue.Items {
					h := health[queue.Items[i].Number]
					queue.Items[i].Observation = &h
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/api/v1/instance" {
						_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
						return
					}
					_ = json.NewEncoder(w).Encode(queue)
				}))
				defer server.Close()
				credential := filepath.Join(t.TempDir(), "credential")
				if err = os.WriteFile(credential, []byte(f.reader), 0600); err != nil {
					t.Fatal(err)
				}
				c, err := client.New(t.Context(), profile.Binding{Context: profile.Context{Profile: "observer-proof", ServerURL: server.URL, InstanceID: f.store.InstanceID(), ProjectID: attemptProject, WorkerID: "reader"}, CredentialFile: credential})
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				got, err := c.ReviewQueue(t.Context(), client.PageOptions{})
				if err != nil {
					t.Fatal("provider error invalidated typed queue", err)
				}
				want := "provider_unavailable"
				if code == "provider_rate_limited" {
					want = code
				}
				found := false
				for _, p := range got.Items {
					if p.Number == 2 {
						found = true
						if p.Observation == nil || p.Observation.State != "retrying" || p.Observation.Error != want {
							t.Fatal("unsafe retry health", p.Observation)
						}
					}
				}
				if !found {
					t.Fatal("target disappeared")
				}
				account := s.accounts[target.account]
				if account.lastErrorCode != want || account.until.Sub(clock.Now()).Seconds() != 120 {
					t.Fatal("sanitization changed cooldown", account)
				}
			})
		}
	}
}
