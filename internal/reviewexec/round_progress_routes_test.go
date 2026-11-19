package reviewexec

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRoundProgressCountsOnlyUnsettledIncomingRoutes(t *testing.T) {
	f, _, evidence := routeFixture(t, 3)
	one := routeCreate(t, f, evidence, "progress-route-one", routeRequest(0, 0, 4))
	two := routeCreate(t, f, evidence, "progress-route-two", routeRequest(1, 1, 4))
	routeCreate(t, f, evidence, "progress-route-other", routeRequest(2, 2, 6))
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	counts := func(want4, want6 int64) {
		t.Helper()
		progress, err := f.e.readRoundProgress(t.Context(), scope, []int64{4, 6, 99}, roundProgressView{incomingRoutes: true})
		if err != nil {
			t.Fatal(err)
		}
		for i, want := range []int64{want4, want6, 0} {
			got := progress.Items[i].IncomingRoutesPending
			if got == nil || *got != want {
				t.Fatalf("PR %d: wanted %d, got %v", progress.Items[i].PullNumber, want, got)
			}
		}
	}
	counts(2, 1)
	routeDecision(t, f, one.ID, "progress-accept", 1, "accepted")
	counts(2, 1)
	routeDecision(t, f, two.ID, "progress-reject", 1, "rejected")
	counts(1, 1)
	routeFix(t, f, one.ID, "progress-fix", 2, "", "dddddddddddddddddddddddddddddddddddddddd")
	counts(0, 1)
	mux := http.NewServeMux()
	f.e.Register(mux)
	readHTTP := func(version string) RoundProgress {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, roundProgressPath(attemptProject)+"?pull_numbers=4,6&view=stages", nil)
		r.Header.Set("Authorization", "Bearer "+f.reader)
		r.Header.Set(reviewFixStatusHeader, version)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		var v RoundProgress
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &v) != nil || len(v.Items) != 2 {
			t.Fatalf("progress: %d %s", w.Code, w.Body.String())
		}
		return v
	}
	if got := readHTTP("v1"); got.Items[0].IncomingRoutesPending != nil || got.Items[1].IncomingRoutesPending != nil {
		t.Fatalf("v1 changed: %+v", got)
	}
	if got := readHTTP("v2"); got.Items[0].IncomingRoutesPending == nil || *got.Items[0].IncomingRoutesPending != 0 || got.Items[1].IncomingRoutesPending == nil || *got.Items[1].IncomingRoutesPending != 1 {
		t.Fatalf("v2 counts: %+v", got)
	}
	if _, err := f.store.DB().Exec(`UPDATE review_finding_route_revisions SET body_json='{}' WHERE route_id=? AND revision=1`, one.ID); err != nil {
		t.Fatal(err)
	}
	if got := readHTTP("v2"); got.Items[0].IncomingRoutesPending != nil || got.Items[1].IncomingRoutesPending == nil || *got.Items[1].IncomingRoutesPending != 1 {
		t.Fatalf("invalidated target must stay unknown independently: %+v", got)
	}

	old, err := f.e.ReadRoundProgressStages(t.Context(), scope, []int64{4})
	if err != nil || old.Items[0].IncomingRoutesPending != nil {
		t.Fatalf("older response changed: %+v, %v", old, err)
	}
}
