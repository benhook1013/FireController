package client_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestCIWatchPendingRequiresTimeoutIncludingZeroWaitAndRearm(t *testing.T) {
	for _, rearm := range []bool{false, true} {
		for _, timedOut := range []bool{false, true} {
			pin := ciWatchPins()[0]
			item := client.CIWatchTargetResult{PullNumber: pin.PullNumber, HeadSHA: pin.HeadSHA, BaseSHA: pin.BaseSHA, State: "pending", Reason: "ci_watch_check_pending", Checks: []client.ReviewCIContext{}}
			if rearm {
				pin.AfterObservationKey = strings.Repeat("d", 64)
				item = ciWatchSample().Targets[1]
				pin.PullNumber, item.PullNumber = 4, 4
				pin.HeadSHA, item.HeadSHA = ciWatchPins()[1].HeadSHA, ciWatchPins()[1].HeadSHA
				item.BaseSHA = pin.BaseSHA
				item.ObservationKey = pin.AfterObservationKey
				item.WaitingForChange = true
			}
			result := client.CIWatchResult{ProjectID: "project-one", State: "pending", TimedOut: timedOut, Targets: []client.CIWatchTargetResult{item}}
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				w.Header().Set("X-FC-CI-Watch", "change-v3")
				write(w, 200, result)
			}))
			c := newClient(t, service)
			_, err := c.WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
			service.Close()
			if timedOut {
				if err != nil {
					t.Fatal("valid expired pending result refused", rearm, err)
				}
			} else {
				checkError(t, err, "invalid_response")
			}
		}
	}
}
