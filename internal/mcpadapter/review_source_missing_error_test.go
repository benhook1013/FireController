package mcpadapter_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/profile"
)

func TestMissingSourceAssessmentMCPKeepsRecoveryAndDoesNotRefresh(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			binding := localContext()
			reads := 0
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/instance" {
					_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": binding.InstanceID})
					return
				}
				reads++
				if r.Method != "GET" || r.URL.Path != "/api/v1/projects/"+binding.ProjectID+"/reviews/source/3356/policy" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != binding.WorkerID {
					t.Error("assessment started work or lost binding")
				}
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "reviews_source_unavailable", "message": "PRIVATE_SERVER_PROSE"}, "request_id": "request-source-missing"})
			}))
			defer service.Close()
			binding.ServerURL = service.URL
			session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, credential(t)), "chosen")
			d := diagnosticResult(t, call(t, session, "review_source_policy_assess", map[string]any{"pull_number": 3356}), "reviews_source_unavailable", false)
			if reads != 1 || strings.Contains(d.Message+d.Action, "PRIVATE_SERVER_PROSE") {
				t.Fatal("assessment retried, fetched or leaked source", d, reads)
			}
			if status == http.StatusConflict {
				if !strings.Contains(d.Action, "already-pending original refresh") || !strings.Contains(d.Action, "review_source_refresh_read") || !strings.Contains(d.Action, "Reassess after the refresh completes") || strings.Contains(d.Action, "Check the selected instance") {
					t.Fatal("MCP masked supported recovery", d)
				}
			} else if strings.Contains(d.Action, "review_source_refresh") {
				t.Fatal("authorization refusal became source-refresh guidance", d)
			}
		})
	}
}
