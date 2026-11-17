package client_test

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestCIWatchWorkflowObservationStatusConclusionPairs(t *testing.T) {
	type pair struct {
		status, conclusion string
		valid              bool
	}
	cases := []pair{}
	for _, status := range []string{"queued", "in_progress", "waiting", "pending", "requested"} {
		cases = append(cases, pair{status, "", true}, pair{status, "success", false}, pair{status, "failure", false})
	}
	for _, conclusion := range []string{"success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", "stale", "startup_failure"} {
		cases = append(cases, pair{"completed", conclusion, true})
	}
	cases = append(cases, pair{"completed", "", false}, pair{"completed", "passed", false}, pair{"unknown", "", false}, pair{"unknown", "success", false}, pair{"", "success", false})
	for _, test := range cases {
		t.Run(test.status+"/"+test.conclusion, func(t *testing.T) {
			pin, value := ciWorkflowObservationSample()
			row := &value.Targets[0].WorkflowObservations[0]
			row.Status, row.Conclusion = test.status, test.conclusion
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-FC-CI-Watch", "change-v3")
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				write(w, 200, value)
			}))
			defer service.Close()
			got, err := newClient(t, service).WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
			if !test.valid {
				checkError(t, err, "invalid_response")
				return
			}
			if err != nil || !reflect.DeepEqual(got, value) {
				t.Fatal("supported workflow lifecycle was rejected or rewritten", got, err)
			}
		})
	}
}
