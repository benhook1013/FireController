package mcpadapter_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/profile"
)

func TestHeadSHADiagnosticNamesConstraintWithoutCredentialsOrRequests(t *testing.T) {
	var requests atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer s.Close()
	binding := localContext()
	binding.ServerURL = s.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, "missing-credential"), "chosen")
	for _, tool := range []string{"review_source_refresh", "review_request"} {
		for _, sha := range []string{"9fa20adff7", strings.Repeat("A", 40), strings.Repeat("g", 40), ""} {
			args := sourceMCPArguments()
			args["head_sha"] = sha
			if tool == "review_request" {
				args["channel"] = "cli"
			}
			d := diagnosticResult(t, call(t, session, tool, args), "invalid_arguments", false)
			if !strings.Contains(d.Message, "head_sha") || !strings.Contains(d.Message, "40 lowercase hexadecimal") || !strings.Contains(d.Action, "No request was sent") || strings.Contains(d.Action, "binding") {
				t.Fatal("malformed SHA received unrelated recovery advice", d)
			}
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid input contacted service")
	}
}
