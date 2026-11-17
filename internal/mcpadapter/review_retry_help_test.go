package mcpadapter_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/profile"
)

func TestReviewToolHelpDistinguishesSameRequestRetryFromExtraRounds(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	binding := localContext()
	binding.ServerURL = server.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, "missing-help-credential"), "chosen")
	listed, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"review_start": false, "review_request": false, "review_operation_read": false}
	for _, tool := range listed.Tools {
		if _, ok := want[tool.Name]; !ok {
			continue
		}
		want[tool.Name] = true
		description := strings.ToLower(tool.Description)
		if !strings.Contains(description, "same accepted") || !strings.Contains(description, "cli quota") || !strings.Contains(description, "automatic") || strings.Contains(description, "no automatic next review or retry") {
			t.Fatal("discovery contradicts same-request retry", tool.Name, tool.Description)
		}
		if tool.Name != "review_operation_read" && (!strings.Contains(description, "cli pr or round") || !strings.Contains(description, "original comparison")) {
			t.Fatal("discovery failed to bound the accepted intent", tool.Name, tool.Description)
		}
	}
	for name, found := range want {
		if !found {
			t.Fatal("missing review help", name)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("help discovery contacted the service")
	}
}
