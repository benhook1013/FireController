package reviewprovider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSourceReadGateBlocksNewReadsWithoutNetworkOrCredentialFallback(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = w.Write([]byte(`[]`)) }))
	defer server.Close()
	first, second := testGitHub(server, "same-secret"), testGitHub(server, "same-secret")
	now := time.Now().UTC()
	failure := &Error{Code: "provider_rate_limited", Status: 429, Message: "Retained source throttle.", RetryTiming: RetryTimingEvidence{State: "proven", ObservedAt: now, NotBefore: now.Add(time.Minute)}}
	blocked := true
	gate := func(context.Context) error {
		if blocked {
			return failure
		}
		return nil
	}
	first.SetReadGate(gate)
	second.SetReadGate(gate)
	for _, g := range []*GitHub{first, second} {
		for range 3 {
			_, err := g.IssueComments(t.Context(), "owner/repo", 1)
			if !errors.Is(err, failure) {
				t.Fatal(err)
			}
		}
	}
	if _, _, _, err := second.pullReviewPage(t.Context(), []byte(`{}`)); !errors.Is(err, failure) {
		t.Fatal("GraphQL read escaped gate", err)
	}
	if calls.Load() != 0 {
		t.Fatal("gate contacted provider")
	}
	blocked = false
	if _, err := first.IssueComments(t.Context(), "owner/repo", 1); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("gate retried or duplicated read", calls.Load())
	}
}
