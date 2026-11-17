package reviews

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestQueueObservationHealthFailurePreservesSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		cancel  bool
		status  int
	}{
		{"overload", core.Fail("overloaded", "busy", 503), false, 200},
		{"storage", fmt.Errorf("wrapped: %w", core.Fail("storage_unavailable", "unavailable", 500)), false, 200},
		{"cancelled_overload", core.Fail("overloaded", "busy", 503), true, 503},
		{"cancelled_storage", core.Fail("storage_unavailable", "unavailable", 500), true, 500},
		{"unauthenticated", core.Fail("unauthenticated", "refused", 401), false, 401},
		{"forbidden", fmt.Errorf("wrapped: %w", core.Fail("forbidden", "refused", 403)), false, 403},
		{"invalid", core.Fail("validation_failed", "invalid", 400), false, 400},
		{"unknown", errors.New("unexpected"), false, 500},
		{"deadline", context.DeadlineExceeded, false, 500},
		{"cancellation", context.Canceled, false, 500},
		{"security_status", core.Fail("overloaded", "refused", 403), false, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAPIFixture(t)
			f.observe(t, reviewRequest(0, []PullInput{samplePull(2, "first"), samplePull(3, "second")}), "health-failure")
			base := "/api/v1/projects/" + f.project + "/reviews/queue?limit=1"
			before := f.call("GET", base, f.readToken, "", nil)
			if before.Code != 200 {
				t.Fatal(before.Code, before.Body.String())
			}
			var want QueueResponse
			if err := json.Unmarshal(before.Body.Bytes(), &want); err != nil {
				t.Fatal(err)
			}
			if !want.HasMore || want.Cursor == "" || len(want.Items) != 1 {
				t.Fatal("missing paginated fixture", want)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			called := 0
			reader := func(_ context.Context, project string, pulls []PullSummary) (ObserverHealth, map[int64]PullObservationHealth, error) {
				called++
				if project != f.project || len(pulls) != 1 {
					t.Fatal(project, pulls)
				}
				if tc.cancel {
					cancel()
				}
				// Never expose a partial success returned alongside an error.
				return ObserverHealth{State: "running"}, map[int64]PullObservationHealth{2: {State: "current"}}, tc.failure
			}
			mux := http.NewServeMux()
			Register(mux, f.store, reader)
			req := httptest.NewRequest("GET", base+"&include_observation=true", nil).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+f.readToken)
			got := httptest.NewRecorder()
			metadataFixtureReaders(f.store, mux).ServeHTTP(got, req)
			if got.Code != tc.status || called != 1 {
				t.Fatal(got.Code, called, got.Body.String())
			}
			if tc.status != 200 {
				return
			}
			var actual QueueResponse
			if err := json.Unmarshal(got.Body.Bytes(), &actual); err != nil {
				t.Fatal(err)
			}
			if actual.Observer == nil || *actual.Observer != (ObserverHealth{State: "unavailable"}) {
				t.Fatal(actual.Observer)
			}
			actual.Observer = nil
			if !reflect.DeepEqual(actual, want) {
				t.Fatalf("queue snapshot changed: got %+v, want %+v", actual, want)
			}
		})
	}
}
