package cli_test

import (
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
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func routeCLIFixture(t *testing.T) (*serviceFixture, string, string, string) {
	t.Helper()
	f := setupServiceFixture(t)
	project := f.project(t, "route-client")
	worker := f.worker(t, project, "Builder")
	for _, ensure := range []func(context.Context, *core.Store) error{reviewexec.EnsureSchema, reviewexec.EnsureEvidenceSchema, reviewexec.EnsureAdjudicationSchema} {
		if err := ensure(t.Context(), f.store); err != nil {
			t.Fatal(err)
		}
	}
	engine, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "fixture", Provider: "synthetic", Account: "fixture", Channels: []string{"cli"}}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/", nil)
	request.SetPathValue("project_id", project)
	request.Header.Set("Authorization", "Bearer "+f.admin)
	request.Header.Set("X-FC-Worker", worker)
	scope, err := f.store.AdminScope(request)
	if err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	if _, err = engine.ConfigurePolicy(t.Context(), scope, "route-policy", reviewexec.PolicyConfiguration{ExpectedRevision: &zero, RequiredChannels: []string{"cli"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Configure(t.Context(), scope, "route-config", reviewexec.Configuration{ExpectedRevision: &zero, CapabilityID: "fixture", Mode: "independent", Queue: []int64{2, 4, 6}, Channels: []string{"cli"}}); err != nil {
		t.Fatal(err)
	}
	queued, err := engine.Enqueue(t.Context(), scope, "route-source", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	if json.Unmarshal(queued, &receipt) != nil {
		t.Fatal("receipt")
	}
	attempt, err := engine.StartAttempt(t.Context(), project, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	var repoID int64
	if err = f.store.DB().QueryRow(`SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	anchor := reviewexec.Anchor{RepositoryID: repoID, Repository: "owner/repository", PullNumber: 2, CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), ParentSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("b", 40), PatchSHA256: strings.Repeat("d", 64), BaseRef: "main", ObservedAt: now}
	attempt, err = engine.Admit(t.Context(), project, attempt.ID, attempt.Revision, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	capture, err := engine.RecordReviewEvidence(t.Context(), project, attempt.ID, reviewexec.ReviewEvidenceInput{CaptureID: "review_capture_" + strings.Repeat("e", 32), CaptureSHA256: strings.Repeat("e", 64), CaptureBytes: 1024, StartedAt: now, FinishedAt: now, ExpectedAnchor: anchor, Review: reviewprovider.ParsedReview{Outcome: "completed", Complete: true, ReportedFindings: 2, ReviewedFiles: []string{"changed.go"}, Findings: []reviewprovider.ParsedFinding{{Title: "Original first", Detail: "Immutable first source", Severity: "high"}, {Title: "Original second", Detail: "Immutable second source", Severity: "low"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Advance(t.Context(), project, attempt.ID, attempt.Revision, "completed", "retained_source", ""); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	engine.Register(mux)
	f.handler = mux
	return f, project, worker, capture.ID
}

func TestReviewRoutesCLIActualPublicWorkflowAndReceiverGuard(t *testing.T) {
	f, project, worker, evidence := routeCLIFixture(t)
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("Authorization") != "Bearer "+f.admin || r.URL.Path != "/api/v1/instance" && r.Header.Get("X-FC-Worker") != worker {
			t.Error("route CLI lost profile binding")
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	providers := f.resolverHit.Load()
	create := func(finding, revision, key string) client.ReviewRouteReceipt {
		t.Helper()
		args := []string{"reviews", "route-create", evidence, "--key", key, "--expect-revision", revision, "--finding", finding, "--target-pr", "4", "--reason", "Exact source routing reason"}
		code, out, stderr := mutationInvoke(t, path, "builder", args...)
		var v client.ReviewRouteReceipt
		var fields map[string]any
		if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &v) != nil || json.Unmarshal([]byte(out), &fields) != nil || len(fields) != 4 || v.State != "routed" || v.Revision != 1 || strings.Contains(out, "routing reason") {
			t.Fatal("route create receipt", code, out, stderr)
		}
		return v
	}
	first := create("0", "0", "first-route")
	second := create("1", "1", "second-route")
	if replay := create("0", "0", "first-route"); replay != first {
		t.Fatal("route replay changed source revision")
	}
	var page client.ReviewRoutePage
	code, out, stderr := mutationInvoke(t, path, "builder", "reviews", "routes", "--pr", "4", "--limit", "1")
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &page) != nil || len(page.Items) != 1 || !page.HasMore || strings.Contains(out, "Immutable") {
		t.Fatal("route page", code, out, stderr)
	}
	before := requests.Load()
	code, out, stderr = mutationInvoke(t, path, "builder", "reviews", "routes", "--pr", "4", "--limit", "1", "--cursor", page.Cursor)
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &page) != nil || len(page.Items) != 1 || page.HasMore || requests.Load() != before+2 {
		t.Fatal("explicit route pagination", code, out, stderr)
	}
	mutationSuccess(t, path, "builder", "reviews", "finalize", evidence, "--key", "freeze-source", "--expect-revision", "2")
	mutationSuccess(t, path, "builder", "reviews", "route-decide", first.ID, "--key", "receiver-decision", "--expect-revision", "1", "--disposition", "accepted", "--reason", "Receiver confirms")
	mutationSuccess(t, path, "builder", "reviews", "route-fix", first.ID, "--key", "receiver-proof", "--expect-revision", "2", "--expect-sha=", "--fix-sha", strings.Repeat("f", 40), "--proof", "Independent receiver proof")
	code, out, stderr = mutationInvoke(t, path, "builder", "reviews", "route", first.ID)
	var detail client.ReviewRouteDetail
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &detail) != nil || detail.Route.FixSHA != strings.Repeat("f", 40) || detail.SourceFinding.Detail != "Immutable first source" || detail.SourceFinding.Finding != 0 {
		t.Fatal("receiver proof/source read", code, out, stderr)
	}
	leave := []string{"reviews", "decide", evidence, "--key", "leave-second", "--expect-revision", "3", "--finding", "1", "--disposition", "accepted", "--reason", "Explicit source correction"}
	code, out, stderr = mutationInvoke(t, path, "builder", leave...)
	if code != cli.ExitConflict || out != "" {
		t.Fatal("source replaced route without receiving guard", code, stderr)
	}
	mutationSuccess(t, path, "builder", append(leave, "--expect-route-revision", "1")...)
	code, out, stderr = mutationInvoke(t, path, "builder", "reviews", "route", second.ID)
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &detail) != nil || detail.Route.State != "superseded" || detail.Route.Revision != 2 {
		t.Fatal("superseded route not readable", code, out, stderr)
	}
	code, out, stderr = mutationInvoke(t, path, "builder", "reviews", "adjudication", evidence)
	var state client.ReviewAdjudication
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &state) != nil || !state.Finalized || state.FrozenCounts == nil || state.FrozenCounts.Routed != 2 || state.FrozenCounts.Accepted != 0 || state.Findings[0].RouteID != first.ID || state.Findings[0].FixSHA != "" || state.Findings[1].RouteID != "" || state.Findings[1].Disposition != "accepted" {
		t.Fatal("routing changed frozen counts or conflated receiver proof", code, out, stderr)
	}
	if f.resolverHit.Load() != providers {
		t.Fatal("routing performed provider work")
	}
}

func TestReviewRoutesCLIInvalidArgumentsBeforeCredentials(t *testing.T) {
	id := "review_route_" + strings.Repeat("a", 32)
	cases := [][]string{{"reviews", "route", "../private"}, {"reviews", "routes"}, {"reviews", "routes", "--pr", "02"}, {"reviews", "routes", "--pr", "4", "--cursor", ""}, {"reviews", "routes", "--pr", "4", "--limit", "0"}, {"reviews", "route-decide", id, "--key", "key", "--expect-revision", "0", "--disposition", "accepted", "--reason", "reason"}, {"reviews", "route-fix", id, "--key", "key", "--expect-revision", "1", "--fix-sha", strings.Repeat("f", 40), "--proof", "proof"}, {"reviews", "decide", cliEvidenceID, "--key", "key", "--expect-revision", "0", "--finding", "0", "--disposition", "routed", "--reason", "reason"}}
	base := []string{"reviews", "route-create", cliEvidenceID, "--key", "key", "--expect-revision", "0", "--finding", "0", "--target-pr", "4", "--reason", "reason"}
	for _, suffix := range [][]string{{"--expect-route-revision", "-1"}, {"--expect-route-revision", "01"}, {"--target-pr", "4"}, {"--project", "other"}} {
		cases = append(cases, append(append([]string{}, base...), suffix...))
	}
	for _, args := range cases {
		code, out, stderr := run(append([]string{"--profiles-file", "/unavailable/route-profile"}, args...), map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal("bad route args reached credentials", args, code, stderr)
		}
	}
}
