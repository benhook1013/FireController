package client_test

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestCIWatchFailedResultRequiresPinnedSelection(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		for _, selection := range []string{"head", "test_merge", "wrong_head", "missing"} {
			name := selection
			if waiting {
				name += "_waiting"
			}
			t.Run(name, func(t *testing.T) {
				pin, response := ciWatchChangeSample()
				item := &response.Targets[0]
				if !waiting {
					pin.AfterObservationKey = ""
					item.WaitingForChange = false
					response.State = "failed"
					response.TimedOut = false
				}
				switch selection {
				case "test_merge":
					item.Selection = "test_merge"
					item.CICommitSHA = strings.Repeat("d", 40)
				case "wrong_head":
					item.CICommitSHA = strings.Repeat("d", 40)
				case "missing":
					item.Selection = ""
					item.CICommitSHA = ""
				}
				service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("X-FC-CI-Watch", "change-v3")
					if r.URL.Path == "/api/v1/instance" {
						handshake(w)
						return
					}
					write(w, 200, response)
				}))
				defer service.Close()
				got, err := newClient(t, service).WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
				if selection == "wrong_head" || selection == "missing" {
					checkError(t, err, "invalid_response")
				} else if err != nil || !reflect.DeepEqual(got, response) {
					t.Fatal("valid pinned failure rejected", got, err)
				}
			})
		}
	}
}
