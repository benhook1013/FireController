package client_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestCIWatchFiniteRefusalStagesSurviveClientWithoutAutomaticRetry(t *testing.T) {
	for reason, message := range map[string]string{
		"ci_watch_scope_read":           "scoped authentication read",
		"ci_watch_binding_read":         "source-binding read",
		"ci_watch_capacity":             "capacity is occupied",
		"ci_watch_observer_unavailable": "observer is unavailable",
		"ci_watch_canceled":             "request context ended",
	} {
		t.Run(reason, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-FC-CI-Watch", "change-v3")
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				posts++
				write(w, 503, map[string]any{"error": map[string]any{"code": "overloaded", "message": "PRIVATE_STORAGE_DETAIL", "details": map[string]any{"reason": reason, "sql": "PRIVATE_SQL"}}, "request_id": "request-ci-refusal"})
			}))
			defer server.Close()
			pins := ciWatchPins()
			pins[0].FailureWorkflowIDs = []int64{31}
			_, err := newClient(t, server).WatchCI(t.Context(), pins, 3)
			var failure *client.Error
			if !errors.As(err, &failure) || failure.Code != "overloaded" || failure.Status != 503 || failure.RequestID != "request-ci-refusal" || !strings.Contains(failure.Message, message) || posts != 1 {
				t.Fatal("safe refusal lost its stage or retried", err, posts)
			}
			details, ok := failure.Details.(map[string]any)
			if !ok || len(details) != 2 || details["reason"] != reason || !reflect.DeepEqual(details["targets"], pins) || strings.Contains(failure.Message+failure.Action, "PRIVATE") || strings.Contains(failure.Message, "shutdown") {
				t.Fatal("refusal leaked raw details or invented deployment cause", failure)
			}
			if !strings.Contains(failure.Action, "bounded") || !strings.Contains(failure.Action, "PR/head/base pins") || reason == "ci_watch_capacity" && !strings.Contains(failure.Action, "avoid repeatedly rearming") {
				t.Fatal("refusal lost its bounded read recovery action", failure.Action)
			}
		})
	}
}

func TestCIWatchUnknownRefusalReasonCannotAcquireShutdownRecovery(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-FC-CI-Watch", "change-v3")
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		posts++
		write(w, 503, map[string]any{"error": map[string]any{"code": "overloaded", "message": "private", "details": map[string]any{"reason": "ci_watch_observer_unavailable_extra"}}, "request_id": "request-ci-unknown"})
	}))
	defer server.Close()
	_, err := newClient(t, server).WatchCI(t.Context(), ciWatchPins(), 3)
	var failure *client.Error
	if !errors.As(err, &failure) || failure.Code != "overloaded" || failure.Details != nil || posts != 1 || strings.Contains(failure.Message, "shutdown") {
		t.Fatal("unknown refusal was trusted or retried", err, posts)
	}
}
