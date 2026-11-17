package reviews

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestQueueObservationHealthIsScopedLiveAndNegotiated(t *testing.T) {
	f := newAPIFixture(t)
	f.observe(t, reviewRequest(0, []PullInput{samplePull(2, "observed")}), "health-fixture")
	called := 0
	reader := func(_ context.Context, project string, pulls []PullSummary) (ObserverHealth, map[int64]PullObservationHealth, error) {
		called++
		if project != f.project || len(pulls) != 1 || pulls[0].Number != 2 {
			t.Fatal(project, pulls)
		}
		return ObserverHealth{State: "running", StartedAt: "2026-10-23T00:00:00Z"}, map[int64]PullObservationHealth{2: {State: "current", LastSuccessAt: "2026-10-23T00:01:00Z"}}, nil
	}
	mux := http.NewServeMux()
	Register(mux, f.store, reader)
	f.mux = mux
	base := "/api/v1/projects/" + f.project + "/reviews/queue"
	old := f.call("GET", base, f.readToken, "", nil)
	if old.Code != 200 || called != 0 || strings.Contains(old.Body.String(), `"observer"`) {
		t.Fatal(old.Code, called)
	}
	for _, view := range []string{"", "&view=recent"} {
		got := f.call("GET", base+"?include_observation=true"+view, f.readToken, "", nil)
		if got.Code != 200 || !strings.Contains(got.Body.String(), `"last_success_at":"2026-10-23T00:01:00Z"`) {
			t.Fatal(got.Code, got.Body.String())
		}
	}
	count := called
	if got := f.call("GET", base+"?include_observation=true", "", "", nil); got.Code != 401 || called != count {
		t.Fatal("unauthenticated health callback", got.Code, called)
	}
	if got := f.call("GET", base+"?include_observation=false", f.readToken, "", nil); got.Code != 400 {
		t.Fatal(got.Code)
	}
}
