package mcpadapter_test

import (
	"net/http/httptest"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestPRFollowingMCPDiscoveryValidationAndRealOwnPreferences(t *testing.T) {
	f, handler, _ := reviewControlsFixture(t, true)
	server := httptest.NewServer(handler)
	defer server.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, server.URL), "chosen")
	tools, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, tool := range tools.Tools {
		if tool.Name == "review_following_read" || tool.Name == "review_following_update" {
			found++
			if tool.Annotations == nil || tool.Annotations.ReadOnlyHint != (tool.Name == "review_following_read") || *tool.Annotations.OpenWorldHint {
				t.Fatal("incorrect following annotations")
			}
		}
	}
	if found != 2 {
		t.Fatal("missing follow tools")
	}
	for _, args := range []map[string]any{{}, {"pull_number": 2}, {"pull_number": 2, "following": false, "expected_revision": 0}, {"pull_number": 2, "following": false, "expected_revision": 0, "idempotency_key": "k", "worker_id": "other"}} {
		errorResult(t, call(t, session, "review_following_update", args), "invalid_arguments")
	}
	var before client.ReviewFollowing
	decodeResult(t, call(t, session, "review_following_read", map[string]any{"pull_number": 2}), &before)
	if before.Mode != "auto" || before.WorkerID != f.sender {
		t.Fatal(before)
	}
	args := map[string]any{"pull_number": 2, "expected_revision": before.Revision, "following": false, "idempotency_key": "mcp-mute"}
	var first, replay client.ReviewFollowing
	decodeResult(t, call(t, session, "review_following_update", args), &first)
	decodeResult(t, call(t, session, "review_following_update", args), &replay)
	if first != replay || first.Mode != "muted" {
		t.Fatal(first, replay)
	}
}
