package cli_test

import (
	"bytes"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewLaneDiagnosticOutputFailureIsReadOnly(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "lane-diagnostic-output")
	worker := f.worker(t, project, "Builder")
	handler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/instance":
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
		case "/api/v1/projects/" + project + "/reviews/lanes/cli/diagnostics":
			if r.Method != http.MethodGet || r.Header.Get("Idempotency-Key") != "" {
				t.Error("diagnostic was not a keyless GET")
			}
			_ = json.NewEncoder(w).Encode(client.ReviewLaneDiagnostic{
				ProjectID: project, Channel: "cli", ConfigurationRevision: 1,
				EvaluatedAt: "2026-11-22T01:00:00Z", Scope: "retained_source_local_policy",
				SourceReason: "source_unavailable", Candidates: []client.ReviewLaneCandidateDiagnostic{},
			})
		default:
			t.Error("unexpected diagnostic route", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer handler.Close()
	path := mutationProfiles(t, f, handler.URL, map[string][2]string{"builder": {project, worker}})
	var stderr bytes.Buffer
	code := cli.RunContext(t.Context(), []string{"--profiles-file", path, "--profile", "builder", "reviews", "scheduling", "diagnostics", "--channel", "cli"}, nil, selectionFailWriter{}, &stderr)
	if code != cli.ExitConfig || !strings.Contains(stderr.String(), "Check the output destination before retrying.") || strings.Contains(stderr.String(), "outcome_unknown") || strings.Contains(stderr.String(), "same key") {
		t.Fatal(code, stderr.String())
	}
}
