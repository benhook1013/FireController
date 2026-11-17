//go:build cohortintegration

package reviewexec

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func (r *metadataEngineReader) QualifyMetadataPair(ctx context.Context, id int64, repo string, n [2]int64) (reviewprovider.MetadataPairQualification, error) {
	return r.github.QualifyMetadataPair(ctx, id, repo, n)
}
func TestLifecycleMetadataQualificationActualProvider(t *testing.T) {
	for _, mode := range []string{"matched", "moved", "limited"} {
		t.Run(mode, func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			configureLifecycleTwoPulls(t, f)
			var calls, ordinary atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count := calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer synthetic-cohort-integration" {
					t.Error("wrong credential")
				}
				s.mu.Lock()
				reserved := s.lanes[lifecycleMetadata].projects[attemptProject] && len(s.lanes[lifecycleMetadata].accounts) == 1
				s.mu.Unlock()
				if !reserved {
					t.Error("missing reservation")
				}
				if mode == "limited" {
					w.Header().Set("Retry-After", "17")
					w.WriteHeader(429)
					return
				}
				if r.Method == "POST" && r.URL.Path == "/graphql" {
					var b struct{ Variables struct{ N0, N1 int64 } }
					if json.NewDecoder(r.Body).Decode(&b) != nil {
						t.Error("bad variables")
					}
					repo := map[string]any{"fullDatabaseId": "801", "nameWithOwner": "owner/repo", "defaultBranchRef": map[string]any{"name": "main"}}
					pull := func(n int64) map[string]any {
						return map[string]any{"title": "Probe only", "number": n, "state": "OPEN", "merged": false, "headRefOid": strings.Repeat("a", 40), "headRefName": "feature", "headRepository": repo, "baseRefOid": strings.Repeat("b", 40), "baseRefName": "main", "baseRepository": repo, "changedFiles": 3, "additions": 4, "deletions": 2, "isDraft": false, "mergedAt": nil, "mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN"}
					}
					json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"fullDatabaseId": "801", "nameWithOwner": "owner/repo", "p0": pull(b.Variables.N0), "p1": pull(b.Variables.N1)}, "rateLimit": map[string]any{"cost": 1}}})
					return
				}
				var n int64
				if _, err := fmt.Sscanf(r.URL.Path, "/repos/owner/repo/pulls/%d", &n); err != nil || r.Method != "GET" {
					t.Error("unexpected request")
				}
				repo := map[string]any{"id": 801, "full_name": "owner/repo", "default_branch": "main"}
				title := "Probe only"
				if mode == "moved" && count > 3 {
					title = "Moved"
				}
				json.NewEncoder(w).Encode(map[string]any{"title": title, "number": n, "state": "open", "merged": false, "head": map[string]any{"sha": strings.Repeat("a", 40), "ref": "feature", "repo": repo}, "base": map[string]any{"sha": strings.Repeat("b", 40), "ref": "main", "repo": repo}, "changed_files": 3, "additions": 4, "deletions": 2, "draft": false, "merged_at": nil, "mergeable": true, "mergeable_state": "clean"})
			}))
			defer server.Close()
			g, err := reviewprovider.NewCohortIntegrationFixture(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			r := &metadataEngineReader{github: g, sourceCITestReader: &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
				ordinary.Add(1)
				return lifecyclePull(n), nil
			}}}}
			s.options.QualifyMetadataPair = true
			readers := map[string]TargetReader{"test-capability": r}
			first, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleMetadata)
			if err != nil || first == nil {
				t.Fatal(first, err)
			}
			if err = f.e.observeLifecycleCohort(t.Context(), *first, readers, s); err != nil {
				t.Fatal(err)
			}
			if mode == "limited" {
				if calls.Load() != 1 || ordinary.Load() != 0 || !s.accounts[first.account].until.After(clock.Now()) {
					t.Fatal("limit behavior", calls.Load(), ordinary.Load())
				}
				return
			}
			if calls.Load() != 5 || ordinary.Load() != 2 {
				t.Fatal("probe/ordinary requests", calls.Load(), ordinary.Load())
			}
			for _, n := range []int64{2, 4} {
				d := s.due[lifecycleKey{attemptProject, n}]
				if d.head != lifecyclePull(n).HeadSHA || d.successAt.IsZero() || !d.ci.IsZero() {
					t.Fatal("ordinary authority", d)
				}
			}
		})
	}
}
