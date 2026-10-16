package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

const cliRefreshID = "review_refresh_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func sourceCLIArgs() []string {
	return []string{"reviews", "source-refresh", "--pr", "2", "--head", strings.Repeat("a", 40), "--expect-config", "1", "--key", "explicit-source"}
}

func cliSourceOperation(project string) client.ReviewSourceOperation {
	return client.ReviewSourceOperation{ID: cliRefreshID, ProjectID: project, PullNumber: 2, HeadSHA: strings.Repeat("a", 40), ConfigurationRevision: 1, State: "held", Revision: 3, Reason: "target_changed", CreatedAt: "2026-10-16T00:00:00Z", UpdatedAt: "2026-10-16T00:00:00Z"}
}

func cliSourceAssessment(project string) client.ReviewSourcePolicyAssessment {
	target := client.ReviewSourceObservation{RepositoryID: 1, RepositoryName: "owner/repo", PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ParentSHA: strings.Repeat("b", 40), ObservedAt: "2026-10-16T00:00:00Z"}
	return client.ReviewSourcePolicyAssessment{ProjectID: project, RefreshID: cliRefreshID, Source: "observed_comparison", ConfigurationRevision: 1, PolicyRevision: 1, SourceObservedAt: target.ObservedAt, EvaluatedAt: target.ObservedAt, State: "held", Target: target, Channels: []client.ReviewPolicyAssessmentChannel{{Channel: "cli"}}}
}

func TestReviewSourceCLIPublicQueuedReceiptReplayAndOperation(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t)
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.URL.RawQuery != "" || r.URL.Path != "/api/v1/instance" && r.Header.Get("X-FC-Worker") != worker {
			t.Error("source CLI lost binding")
		}
		if r.Method == "POST" && r.Header.Get("Prefer") != "wait=0" {
			t.Error("explicit zero wait was lost")
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	args := append(sourceCLIArgs(), "--wait-seconds", "0")
	before := f.resolverHit.Load()
	receipt := mutationSuccess(t, path, "builder", args...)
	if len(receipt) != 3 || receipt["state"] != "queued" || receipt["revision"] != float64(1) || requests.Load() != 2 {
		t.Fatal("source request polled or expanded receipt", receipt)
	}
	replay := mutationSuccess(t, path, "builder", args...)
	if encodeString(receipt) != encodeString(replay) {
		t.Fatal("source replay changed receipt")
	}
	code, out, stderr := mutationInvoke(t, path, "builder", "reviews", "source-operation", receipt["id"].(string))
	var operation client.ReviewSourceOperation
	if code != 0 || stderr != "" || strings.Count(out, "\n") != 1 || json.Unmarshal([]byte(out), &operation) != nil || operation.State != "queued" || operation.ProjectID != project || operation.Observation != nil || requests.Load() != 6 || f.resolverHit.Load() != before {
		t.Fatal("operation read refreshed or lost stored facts", code, out, stderr)
	}
}

func TestReviewSourceCLIDirectTerminalDefaultWaitAndStoredAssessment(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "source")
	worker := f.worker(t, project, "Builder")
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+f.admin || r.Header.Get("X-FC-Worker") != worker || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.URL.RawQuery != "" {
			t.Error("source call changed binding")
		}
		base := "/api/v1/projects/" + project + "/reviews/"
		switch r.URL.Path {
		case base + "source-refreshes":
			if r.Method != "POST" || r.Header.Get("Prefer") != "wait=2" || r.Header.Get("Idempotency-Key") != "explicit-source" {
				t.Error("default wait/key lost")
			}
			_ = json.NewEncoder(w).Encode(cliSourceOperation(project))
		case base + "source/2/policy":
			if r.Method != "GET" || r.Header.Get("Prefer") != "" {
				t.Error("assessment requested refresh")
			}
			_ = json.NewEncoder(w).Encode(cliSourceAssessment(project))
		default:
			t.Error("unexpected source route", r.URL.Path)
		}
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	code, out, stderr := mutationInvoke(t, path, "builder", sourceCLIArgs()...)
	var operation client.ReviewSourceOperation
	if code != 0 || stderr != "" || strings.Count(out, "\n") != 1 || json.Unmarshal([]byte(out), &operation) != nil || operation.ID != cliRefreshID || operation.State != "held" || strings.Contains(out, `"Operation"`) || requests.Load() != 2 {
		t.Fatal("direct terminal result was wrapped/polled", code, out, stderr)
	}
	code, out, stderr = mutationInvoke(t, path, "builder", "reviews", "source-assess", "--pr", "2")
	var assessment client.ReviewSourcePolicyAssessment
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &assessment) != nil || assessment.Source != "observed_comparison" || assessment.ProjectID != project || requests.Load() != 4 {
		t.Fatal("assessment did not use one local explicit read", code, out, stderr)
	}
}

func TestReviewSourceCLIInvalidArgumentsBeforeCredentials(t *testing.T) {
	base := sourceCLIArgs()
	cases := [][]string{{"reviews", "source-refresh"}, {"reviews", "source-operation", "../private"}, {"reviews", "source-assess", "2"}, {"reviews", "source-assess", "--pr", "02"}, {"reviews", "source-assess", "--pr", "2", "--refresh"}, {"reviews", "source-operation", cliRefreshID, "--wait-seconds", "0"}}
	for _, suffix := range [][]string{{"--wait-seconds", "-1"}, {"--wait-seconds", "61"}, {"--wait-seconds", "01"}, {"--wait-seconds", "1.5"}, {"--wait-seconds", "0", "--wait-seconds", "1"}, {"--project", "other"}, {"--worker", "other"}, {"--pr", "2"}} {
		args := append([]string{}, base...)
		cases = append(cases, append(args, suffix...))
	}
	for _, args := range cases {
		code, out, stderr := run(append([]string{"--profiles-file", "/unavailable/source-profile"}, args...), map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal("bad source arguments reached credentials", args, code, stderr)
		}
	}
}

func TestReviewSourceCLICancellationKeepsUnknownMutationRecoverable(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "cancel-source")
	worker := f.worker(t, project, "Builder")
	entered, closed := make(chan struct{}, 1), make(chan struct{}, 1)
	shutdown := make(chan struct{})
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-shutdown:
		}
		closed <- struct{}{}
	}))
	defer service.Close()
	defer close(shutdown)
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var out, stderr bytes.Buffer
	done := make(chan int, 1)
	args := append([]string{"--profiles-file", path, "--profile", "builder"}, sourceCLIArgs()...)
	args = append(args, "--wait-seconds", "60")
	go func() { done <- cli.RunContext(ctx, args, nil, &out, &stderr) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("source request did not enter HTTP")
	}
	cancel()
	select {
	case code := <-done:
		if out.Len() != 0 || code == 0 || !strings.Contains(stderr.String(), `"outcome_unknown":true`) {
			t.Fatal("cancellation lost unknown receipt recovery", code, stderr.String())
		}
	case <-time.After(time.Second):
		t.Fatal("source wait ignored cancellation")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cancelled source CLI left HTTP open")
	}
}
