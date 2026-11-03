package mcpadapter_test

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/profile"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRefreshIDDiagnosticNamesFormatBeforeCredentialsOrNetwork(t *testing.T) {
	var requests atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer s.Close()
	binding := localContext()
	binding.ServerURL = s.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, "missing-credential"), "chosen")
	for _, id := range []string{"", strings.Repeat("a", 32), "review_refresh_" + strings.Repeat("A", 32), "review_refresh_" + strings.Repeat("g", 32), "review_operation_" + strings.Repeat("a", 32), "PRIVATE_INVALID_REFRESH_ID", strings.Repeat("s", 4000)} {
		d := diagnosticResult(t, call(t, session, "review_source_refresh_read", map[string]any{"refresh_id": id}), "invalid_arguments", false)
		b, _ := json.Marshal(d)
		if !strings.Contains(d.Message, "refresh_id") || !strings.Contains(d.Message, "32 lowercase hexadecimal") || !strings.Contains(d.Action, "No request was sent") || !strings.Contains(string(b), `"field":"refresh_id"`) || strings.Contains(string(b), "PRIVATE_INVALID_REFRESH_ID") || len(b) > 1024 || strings.Contains(d.Action, "binding") {
			t.Fatal("invalid ID lost field-specific recovery", string(b))
		}
	}
	if requests.Load() != 0 {
		t.Fatal("malformed ID contacted the service")
	}
}
