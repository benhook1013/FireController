package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewSelectionBusyCLIExposesReadableOriginalHandle(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t)
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	profiles := mutationProfiles(t, f, srv.URL, map[string][2]string{"builder": {project, worker}})
	first := mutationSuccess(t, profiles, "builder", "reviews", "start", "--channel", "cli", "--key", "accepted-start", "--wait-seconds", "0")
	id := first["id"].(string)
	counts := func() [3]int64 {
		var out [3]int64
		for i, table := range []string{"review_selections", "review_operations", "receipts"} {
			if err := f.store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&out[i]); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	before, providers := counts(), f.resolverHit.Load()
	for range 2 {
		code, out, stderr := mutationInvoke(t, profiles, "builder", "reviews", "start", "--channel", "cli", "--key", "refused-start", "--expect-pr", "3386", "--wait-seconds", "0")
		var response struct {
			Error client.Error `json:"error"`
		}
		if code == 0 || out != "" || json.Unmarshal([]byte(stderr), &response) != nil || response.Error.Code != "review_busy" || response.Error.OutcomeUnknown || !strings.Contains(response.Error.Message, "not accepted") || !strings.Contains(response.Error.Action, "reviews run-read --selection-id "+id) {
			t.Fatal("CLI busy response hid selection or fabricated acceptance", code, out, stderr)
		}
		details, ok := response.Error.Details.(map[string]any)
		if !ok || details["selection_id"] != id || details["channel"] != "cli" || details["state"] != "queued" || counts() != before {
			t.Fatal("CLI dropped or changed original pending work", stderr)
		}
	}
	code, out, stderr := mutationInvoke(t, profiles, "builder", "reviews", "run-read", "--selection-id", id)
	var state client.ReviewRunState
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &state) != nil || state.Selection.ID != id || state.Operation != nil || counts() != before || posts.Load() != 3 || f.resolverHit.Load() != providers {
		t.Fatal("CLI recovery read started extra work", code, out, stderr)
	}
}
