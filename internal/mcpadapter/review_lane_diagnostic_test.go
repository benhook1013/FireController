package mcpadapter_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestLaneDiagnosticSDKReadOnlyNoProviderWork(t *testing.T) {
	f, handler, _ := reviewControlsFixture(t)
	server := httptest.NewServer(handler)
	defer server.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, server.URL), "chosen")
	listed, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range listed.Tools {
		if tool.Name == "review_lane_diagnostic" {
			found = true
			a := tool.Annotations
			if a == nil || !a.ReadOnlyHint || !a.IdempotentHint || a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
				t.Fatal("incorrect read effects")
			}
		}
	}
	if !found {
		t.Fatal("missing diagnostic tool")
	}
	before := f.providers.Load()
	for _, channel := range []string{"cli", "hosted"} {
		var out client.ReviewLaneDiagnostic
		decodeResult(t, call(t, session, "review_lane_diagnostic", map[string]any{"channel": channel}), &out)
		if out.Channel != channel || out.Scope != "retained_source_local_policy" || out.SourceComplete || out.SourceReason != "source_unavailable" {
			t.Fatal(out)
		}
	}
	for _, raw := range []string{`{}`, `null`, `{"channel":"manual"}`, `{"channel":"cli","refresh":true}`, `{"channel":"cli","channel":"hosted"}`, `{"channel":"cli","project_id":"foreign"}`} {
		diagnosticResult(t, call(t, session, "review_lane_diagnostic", json.RawMessage(raw)), "invalid_arguments", false)
	}
	if f.providers.Load() != before {
		t.Fatal("diagnostic contacted provider")
	}
}
