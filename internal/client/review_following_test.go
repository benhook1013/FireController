package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewFollowingClientBoundReadWriteAndStrictResponse(t *testing.T) {
	var calls atomic.Int64
	var malformed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.URL.Path != "/api/v1/projects/project-one/reviews/pulls/42/following" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("X-FC-Instance") != "instance-one" || r.URL.RawQuery != "" {
			t.Error("unbound following request")
		}
		mode := "none"
		revision := int64(0)
		if r.Method == "PUT" {
			var input client.ConfigureReviewFollowingRequest
			if json.NewDecoder(r.Body).Decode(&input) != nil || input.ExpectedRevision != 0 || !input.Following || r.Header.Get("Idempotency-Key") != "follow-key" {
				t.Error("changed following write")
			}
			mode, revision = "follow", 1
		}
		value := map[string]any{"project_id": "project-one", "pull_number": 42, "worker_id": "worker-one", "mode": mode, "revision": revision, "tracked": true}
		if malformed.Load() {
			value["worker_id"] = "other-worker"
		}
		write(w, 200, value)
	}))
	defer server.Close()
	c := newClient(t, server)
	v, err := c.ReviewFollowing(context.Background(), 42)
	if err != nil || v.Mode != "none" || v.Revision != 0 {
		t.Fatal(v, err)
	}
	v, err = c.ConfigureReviewFollowing(context.Background(), "follow-key", 42, client.ConfigureReviewFollowingRequest{Following: true})
	if err != nil || v.Mode != "follow" || v.Revision != 1 {
		t.Fatal(v, err)
	}
	before := calls.Load()
	if _, err = c.ReviewFollowing(context.Background(), 0); err == nil {
		t.Fatal("invalid PR accepted")
	}
	if _, err = c.ConfigureReviewFollowing(context.Background(), "", 42, client.ConfigureReviewFollowingRequest{}); err == nil {
		t.Fatal("missing key accepted")
	}
	if calls.Load() != before {
		t.Fatal("invalid arguments reached transport")
	}
	malformed.Store(true)
	if _, err = c.ReviewFollowing(context.Background(), 42); err == nil {
		t.Fatal("foreign worker response accepted")
	}
	_, err = c.ConfigureReviewFollowing(context.Background(), "follow-key", 42, client.ConfigureReviewFollowingRequest{Following: true})
	var failure *client.Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown {
		t.Fatal("invalid committed response lost unknown-write recovery", err)
	}
}
