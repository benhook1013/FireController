package client_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestCIWatchShutdownRecoveryUsesOriginalComparison(t *testing.T) {
	for _, mode := range []string{"explicit_immediate", "explicit_exhausted", "implicit", "mixed"} {
		t.Run(mode, func(t *testing.T) {
			pins := ciWatchPins()
			pins[1].BaseSHA = strings.Repeat("d", 40)
			pins[0].AfterObservationKey = strings.Repeat("c", 64)
			pins[0].FailureWorkflowIDs = []int64{31}
			seconds, expectedPosts := 0, 1
			switch mode {
			case "explicit_exhausted":
				seconds, expectedPosts = 1, 2
			case "implicit":
				seconds = 3
				for i := range pins {
					pins[i].BaseSHA = ""
				}
			case "mixed":
				seconds = 3
				pins[0].BaseSHA = ""
			}
			var posts atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				posts.Add(1)
				var input struct {
					Targets []client.CIWatchTarget `json:"targets"`
				}
				if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&input) != nil || !reflect.DeepEqual(input.Targets, pins) {
					t.Error("CI watch or reconnect changed its wire comparison")
					write(w, http.StatusBadRequest, nil)
					return
				}
				write(w, 503, map[string]any{"error": map[string]any{"code": "overloaded", "message": "PRIVATE", "details": map[string]any{"reason": "result_wait_lifetime"}}, "request_id": "request-ci-shutdown"})
			}))
			defer server.Close()
			_, err := newClient(t, server).WatchCI(t.Context(), pins, seconds)
			var failure *client.Error
			if !errors.As(err, &failure) || failure.Code != "overloaded" || failure.Status != 503 || failure.RequestID != "request-ci-shutdown" || posts.Load() != int64(expectedPosts) {
				t.Fatal("shutdown lost original refusal or reconnect boundary", err, posts.Load())
			}
			details, ok := failure.Details.(map[string]any)
			if !ok || len(details) != 2 || details["reason"] != "result_wait_lifetime" || !reflect.DeepEqual(details["targets"], pins) {
				t.Fatal("shutdown lost original comparison and workflow/baseline selection", failure)
			}
			if !strings.Contains(failure.Message, "shutdown") || strings.Contains(failure.Message+failure.Action, "PRIVATE") || strings.Contains(failure.Action, "operation ID") {
				t.Fatal("CI watch recovery invented an operation or lost safe shutdown facts", failure)
			}
			if mode == "implicit" || mode == "mixed" {
				if !strings.Contains(failure.Action, "base was implicit") || !strings.Contains(failure.Action, "Verify the original comparison") || !strings.Contains(failure.Action, "explicit base pins") {
					t.Fatal("shutdown lost the implicit-base warning", failure.Action)
				}
			} else if !strings.Contains(failure.Action, "bounded CI wait") || !strings.Contains(failure.Action, "original PR/head/base pins") || !strings.Contains(failure.Action, "does not establish CI failure") {
				t.Fatal("shutdown retained durable-operation recovery", failure.Action)
			}
		})
	}
}
