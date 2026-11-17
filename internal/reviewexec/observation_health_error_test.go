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

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

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
