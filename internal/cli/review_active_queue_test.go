package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestReviewActiveQueueCLICompactViewAndContinuation(t *testing.T) {
	f := setupServiceFixture(t)
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	reviews.Register(mux, f.store)
	f.handler = mux
	project := f.project(t, "active-cli")
	worker := f.worker(t, project, "Builder")
	f.call(t, "POST", "/api/v1/projects/"+project+"/reviews/observations", f.admin, "", reviewObservation())
	var activeReads, writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/reviews/queue/active") {
			activeReads.Add(1)
		} else if strings.Contains(r.URL.Path, "/reviews/") && !strings.HasSuffix(r.URL.Path, "/reviews/queue") {
			t.Errorf("unexpected queue fanout: %s", r.URL.Path)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	profiles := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	invoke := func(dst any, args ...string) string {
		t.Helper()
		code, out, diagnostic := mutationInvoke(t, profiles, "builder", append([]string{"reviews", "queue"}, args...)...)
		if code != cli.ExitSuccess || diagnostic != "" || json.Unmarshal([]byte(out), dst) != nil {
			t.Fatal(code, out, diagnostic)
		}
		return out
	}
	var first client.ActiveQueueResponse
	out := invoke(&first, "--active")
	if len(first.Items) != 5 || first.Items[0].Number != 2 || first.Items[0].Position != 1 || first.HasMore || strings.Contains(out, "PRIVATE_FINDING_DETAIL") || strings.Contains(out, `"observed_at"`) || strings.Contains(out, `"recent_runs"`) {
		t.Fatal("incorrect compact active queue", out)
	}
	var page, tail client.ActiveQueueResponse
	invoke(&page, "--active", "--limit", "2")
	if len(page.Items) != 2 || page.Items[0].Number != 2 || page.Items[1].Number != 3 || !page.HasMore {
		t.Fatal(page)
	}
	invoke(&tail, "--active", "--limit", "100", "--cursor", page.Cursor)
	if tail.SnapshotID != page.SnapshotID || len(tail.Items) != 3 || tail.Items[0].Number != 4 || tail.Items[2].Number != 6 || tail.HasMore {
		t.Fatal(tail)
	}
	var full client.ReviewQueue
	invoke(&full)
	if full.Items[0].State != "merged" || full.Items[0].Number != 1 {
		t.Fatal("default full view changed")
	}
	if writes.Load() != 0 || activeReads.Load() != 3 || f.resolverHit.Load() != 1 {
		t.Fatal("active queue wrote, refetched, or performed provider work", writes.Load(), activeReads.Load())
	}
}

func TestReviewActiveQueueCLIRejectsArgumentsBeforeProfile(t *testing.T) {
	for _, args := range [][]string{{"--active", "--active"}, {"--active=false"}, {"--active=true"}, {"--active", "extra"}, {"--active", "--limit", "0"}, {"--active", "--limit", "101"}, {"--active", "--limit", "2", "--limit", "3"}, {"--active", "--cursor", ""}, {"--active", "--cursor", "--active"}, {"--active", "--snapshot", "review_snapshot_" + strings.Repeat("a", 32)}} {
		code, out, diagnostic := mutationInvoke(t, "missing-active-queue-profiles", "builder", append([]string{"reviews", "queue"}, args...)...)
		if code != cli.ExitUsage || out != "" || !strings.Contains(diagnostic, "invalid_arguments") {
			t.Fatal(args, code, out, diagnostic)
		}
	}
}
