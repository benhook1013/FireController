//go:build cohortintegration

package reviewexec

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type metadataEngineReader struct {
	*sourceCITestReader
	github *reviewprovider.GitHub
}

func (r *metadataEngineReader) PullMetadataPair(ctx context.Context, id int64, repo string, n [2]int64) [2]reviewprovider.PullMetadataPairResult {
	return r.github.PullMetadataPair(ctx, id, repo, n)
}
func TestLifecycleMetadataPairActualProviderPublication(t *testing.T) {
	for _, mode := range []string{"enabled", "disabled", "hint", "late_hint", "rate_limit", "partial_failure", "partial_late_hint", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			configureLifecycleTwoPulls(t, f)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			release := make(chan struct{})
			var pairCalls, ordinary atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				pairCalls.Add(1)
				if req.Method != "POST" || req.URL.Path != "/graphql" || req.Header.Get("Authorization") != "Bearer synthetic-cohort-integration" {
					t.Error("unexpected request/credential")
				}
				s.mu.Lock()
				reserved := s.lanes[lifecycleMetadata].projects[attemptProject] && len(s.lanes[lifecycleMetadata].accounts) == 1
				s.mu.Unlock()
				if !reserved {
					t.Error("missing existing reservation")
				}
				if mode == "canceled" {
					cancel()
					<-release
					return
				}
				if mode == "rate_limit" {
					w.Header().Set("Retry-After", "17")
					w.WriteHeader(429)
					return
				}
				var body struct{ Variables struct{ N0, N1 int64 } }
				if json.NewDecoder(req.Body).Decode(&body) != nil {
					t.Error("request variables")
				}
				repo := map[string]any{"fullDatabaseId": "801", "nameWithOwner": "owner/repo", "defaultBranchRef": map[string]any{"name": "main"}}
				pull := func(n int64) map[string]any {
					return map[string]any{"title": fmt.Sprintf("Paired %d", n), "number": n, "state": "OPEN", "merged": false, "headRefOid": strings.Repeat("a", 40), "headRefName": fmt.Sprintf("feature-%d", n), "headRepository": repo, "baseRefOid": strings.Repeat("b", 40), "baseRefName": "main", "baseRepository": repo, "changedFiles": 1, "additions": 2, "deletions": 1, "isDraft": false, "mergedAt": nil, "mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN"}
				}
				if mode == "late_hint" || mode == "partial_late_hint" {
					s.mu.Lock()
					key := lifecycleKey{attemptProject, body.Variables.N0}
					d := s.due[key]
					d.hints[lifecycleMetadata].wanted++
					s.due[key] = d
					s.mu.Unlock()
				}
				if mode == "partial_failure" || mode == "partial_late_hint" {
					json.NewEncoder(w).Encode(map[string]any{"data": nil, "errors": []any{map[string]any{"type": "FORBIDDEN"}}})
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"fullDatabaseId": "801", "nameWithOwner": "owner/repo", "p0": pull(body.Variables.N0), "p1": pull(body.Variables.N1)}, "rateLimit": map[string]any{"cost": 1}}})
			}))
			defer server.Close()
			defer close(release)
			g, err := reviewprovider.NewCohortIntegrationFixture(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			r := &metadataEngineReader{github: g, sourceCITestReader: &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
				ordinary.Add(1)
				return lifecyclePull(n), nil
			}}}}
			s.options.BatchMetadata = mode != "disabled"
			if mode == "hint" {
				s.due[lifecycleKey{attemptProject, 2}] = lifecycleDue{hints: [2]lifecycleHint{{wanted: 1}}}
			}
			readers := map[string]TargetReader{"test-capability": r}
			first, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleMetadata)
			if err != nil || first == nil {
				t.Fatal(first, err)
			}
			if err = f.e.observeLifecycleCohort(ctx, *first, readers, s); err != nil {
				t.Fatal(err)
			}
			if len(s.lanes[lifecycleMetadata].accounts) != 0 || len(s.lanes[lifecycleMetadata].projects) != 0 {
				t.Fatal("reservation leak")
			}
			if mode == "disabled" || mode == "hint" {
				if pairCalls.Load() != 0 || ordinary.Load() != 1 {
					t.Fatal("individual path changed", pairCalls.Load(), ordinary.Load())
				}
				return
			}
			if pairCalls.Load() != 1 || ordinary.Load() != 0 {
				t.Fatal("unexpected extra reads", pairCalls.Load(), ordinary.Load())
			}
			for _, n := range []int64{2, 4} {
				d := s.due[lifecycleKey{attemptProject, n}]
				if mode == "partial_failure" || mode == "partial_late_hint" {
					if !d.next.Equal(clock.Now().Add(time.Minute)) || d.lastErrorCode == "" || !d.successAt.IsZero() || d.head != "" || !d.ci.IsZero() {
						t.Fatal("malformed retry cadence or false success", d)
					}
					continue
				}
				if mode == "rate_limit" || mode == "canceled" {
					if !d.next.IsZero() || !d.checkedAt.IsZero() {
						t.Fatal("failure changed per-target due", d)
					}
					continue
				}
				if !d.next.Equal(clock.Now().Add(time.Minute)) || d.head != strings.Repeat("a", 40) || d.baseSHA != strings.Repeat("b", 40) {
					t.Fatal("metadata not published at ordinary cadence", n, d)
				}
				if !d.ci.IsZero() {
					t.Fatal("metadata manufactured CI freshness")
				}
			}
			if mode == "partial_failure" {
				again, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleMetadata)
				if err != nil || again != nil || pairCalls.Load() != 1 {
					t.Fatal("malformed pair immediately reclaimed", again, err, pairCalls.Load())
				}
			}
			if mode == "rate_limit" && !s.accounts[first.account].until.After(clock.Now()) {
				t.Fatal("account cooldown lost")
			}
			if mode == "late_hint" || mode == "partial_late_hint" {
				h := s.due[lifecycleKey{attemptProject, first.number}].hints[lifecycleMetadata]
				if h.wanted == h.done {
					t.Fatal("late hint consumed")
				}
			}
		})
	}
}
