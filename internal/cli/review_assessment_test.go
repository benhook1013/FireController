package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func cliPolicyAssessment(project string) client.ReviewPolicyAssessment {
	now := "2026-10-16T00:00:00Z"
	return client.ReviewPolicyAssessment{ProjectID: project, EvidenceID: cliEvidenceID, Source: "captured_comparison", PolicyRevision: 1, EvaluatedAt: now, State: "held", Channels: []client.ReviewPolicyAssessmentChannel{{Channel: "cli", Pending: true}}, Anchor: client.ReviewAnchor{RepositoryID: 1, Repository: "owner/repo", PullNumber: 2, CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), ParentSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("c", 40), PatchSHA256: strings.Repeat("d", 64), BaseRef: "main", ObservedAt: now}}
}

func TestReviewPolicyAssessmentCLIBoundReadAndArgumentPrecedence(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "assessment")
	worker := f.worker(t, project, "Builder")
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer "+f.admin || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.URL.RawQuery != "" {
			t.Error("CLI changed assessment authority or requested provider refresh")
		}
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		if r.Header.Get("X-FC-Worker") != worker || r.URL.Path != "/api/v1/projects/"+project+"/reviews/evidence/"+cliEvidenceID+"/policy" {
			t.Error("CLI assessment lost selected profile binding")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cliPolicyAssessment(project))
	}))
	defer srv.Close()
	path := mutationProfiles(t, f, srv.URL, map[string][2]string{"builder": {project, worker}})
	code, out, stderr := mutationInvoke(t, path, "builder", "reviews", "assess", cliEvidenceID)
	var result client.ReviewPolicyAssessment
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &result) != nil || result.State != "held" || result.Source != "captured_comparison" || strings.Count(out, "\n") != 1 || calls.Load() != 2 {
		t.Fatal("assessment was not one quiet explicit read", code, out, stderr)
	}
	var explained client.ReviewPolicyAssessmentOutput
	if json.Unmarshal([]byte(out), &explained) != nil || len(explained.Channels) != 1 || explained.Channels[0].Reason != "review_work_pending" || explained.Channels[0].NextAction != "finish_retained_review_work" || !strings.Contains(out, `"rounds_complete":false`) {
		t.Fatal("CLI omitted the channel hold or explicit false", out)
	}
	before := calls.Load()
	for _, args := range [][]string{
		{"reviews", "assess"}, {"reviews", "assess", "../private"}, {"reviews", "assess", cliEvidenceID, "extra"},
		{"reviews", "assess", cliEvidenceID, "--snapshot", "review_snapshot_" + strings.Repeat("a", 32)},
		{"reviews", "assess", cliEvidenceID, "--project", project}, {"reviews", "assess", cliEvidenceID, "--worker", worker},
		{"reviews", "assess", cliEvidenceID, "--limit", "1"}, {"reviews", "assess", cliEvidenceID, "--refresh"},
		{"reviews", "assess", "review_run_" + strings.Repeat("a", 32)},
	} {
		code, out, stderr := run(append([]string{"--profiles-file", "/unavailable/assessment-profile"}, args...), map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal("invalid assessment reached credential loading", args, code, stderr)
		}
	}
	if calls.Load() != before {
		t.Fatal("invalid arguments performed HTTP")
	}
}

func TestReviewPolicyAssessmentCLIRespectsCallerWholeBudget(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "assessment-budget")
	worker := f.worker(t, project, "Builder")
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-time.After(2700 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		_ = json.NewEncoder(w).Encode(cliPolicyAssessment(project))
	}))
	defer srv.Close()
	path := mutationProfiles(t, f, srv.URL, map[string][2]string{"builder": {project, worker}})
	var out, stderr bytes.Buffer
	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	code := cli.RunContext(ctx, []string{"--profiles-file", path, "reviews", "assess", cliEvidenceID}, map[string]string{"FC_PROFILE": "builder"}, &out, &stderr)
	elapsed := time.Since(start)
	if code != cli.ExitCancelled || out.Len() != 0 || !strings.Contains(stderr.String(), "deadline_exceeded") || elapsed < 4500*time.Millisecond || elapsed > 6*time.Second || calls.Load() != 2 {
		t.Fatal("handshake+assessment acquired separate budgets", code, elapsed, stderr.String())
	}
}
