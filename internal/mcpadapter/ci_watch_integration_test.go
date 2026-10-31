package mcpadapter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCIWatchSDKReadRouteResultAndLongCancellation(t *testing.T) {
	identity := localContext()
	pins := []client.CIWatchTarget{{PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40)}, {PullNumber: 4, HeadSHA: strings.Repeat("c", 40)}}
	entered, closed := make(chan struct{}, 1), make(chan struct{}, 1)
	var posts atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			jsonResponse(w, map[string]string{"instance_id": identity.InstanceID})
			return
		}
		posts.Add(1)
		var body struct {
			Targets []client.CIWatchTarget `json:"targets"`
		}
		if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/ci/watch" || r.URL.RawQuery != "" || r.Header.Get("X-FC-Worker") != identity.WorkerID || r.Header.Get("Idempotency-Key") != "" || json.NewDecoder(r.Body).Decode(&body) != nil || !reflect.DeepEqual(body.Targets, pins) {
			t.Error("watch read lost exact route/targets or became a keyed mutation")
		}
		if r.Header.Get("Prefer") == "wait=900" {
			entered <- struct{}{}
			<-r.Context().Done()
			closed <- struct{}{}
			return
		}
		if r.Header.Get("Prefer") != "wait=0" {
			t.Error("immediate wait changed")
		}
		jsonResponse(w, client.CIWatchResult{ProjectID: identity.ProjectID, State: "passed", Targets: []client.CIWatchTargetResult{
			{PullNumber: 2, HeadSHA: pins[0].HeadSHA, BaseSHA: pins[0].BaseSHA, State: "pending", Reason: "ci_required_checks_pending", ObservedAt: "2026-11-01T00:00:00Z", Required: 1, Pending: 1, Checks: []client.ReviewCIContext{{Context: "required-build", State: "pending", Reason: "ci_status_pending", CheckRun: &client.ReviewCICheckRun{ID: 71, Status: "completed", Conclusion: "success"}, CommitStatus: &client.ReviewCICommitStatus{ID: 91, State: "pending"}}}},
			{PullNumber: 4, HeadSHA: pins[1].HeadSHA, State: "passed", BaseRef: "stack-parent", GateRef: "main", ObservedHeadSHA: pins[1].HeadSHA, ObservedBaseSHA: strings.Repeat("b", 40), ObservedAt: "2026-11-01T00:00:00Z", Selection: "head", CICommitSHA: pins[1].HeadSHA, Required: 1, Satisfied: 1, Checks: []client.ReviewCIContext{{Context: "required-build", State: "satisfied"}}},
		}})
	}))
	defer service.Close()
	identity.ServerURL = service.URL
	profilePath := profiles(t, map[string]profile.Context{"chosen": identity}, credential(t))
	session := connect(t, adapter(t, profilePath, "chosen"), "")
	tools, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name == "wait_for_ci" {
			found = true
			if !strings.Contains(tool.Description, "all_targets_passed") || !strings.Contains(tool.Description, "wake condition") {
				t.Fatal("CI wait help implies batch success", tool.Description)
			}
			if !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint || tool.Annotations.OpenWorldHint == nil || !*tool.Annotations.OpenWorldHint {
				t.Fatal("dishonest CI watch annotations")
			}
		}
	}
	if !found {
		t.Fatal("CI watch not registered")
	}
	var result client.CIWatchOutput
	decodeResult(t, call(t, session, "wait_for_ci", map[string]any{"targets": pins, "wait_seconds": 0}), &result)
	if result.State != "passed" || result.AllTargetsPassed || len(result.Targets) != 2 || result.Targets[1].GateRef != "main" || result.Targets[1].Checks[0].Context != "required-build" {
		t.Fatal("compact CI result lost actionable facts", result)
	}
	if result.Targets[0].Phase != "checks_pending" || result.Targets[0].Diagnostic == nil || !strings.Contains(result.Targets[0].Diagnostic.Explanation, "commit status 91") || !strings.Contains(result.Targets[0].Diagnostic.Explanation, "check run 71") || !strings.Contains(result.Targets[0].Diagnostic.Explanation, "pending, not a CI failure") || !strings.Contains(result.Targets[0].Diagnostic.NextAction, "do not rerun CI") || result.Targets[0].Required != 1 || result.Targets[0].Pending != 1 || result.Targets[1].Diagnostic != nil {
		t.Fatal("pending commit status was unexplained or terminal facts changed", result)
	}
	var out, diag bytes.Buffer
	cliArgs := []string{"--profiles-file", profilePath, "--profile", "chosen", "reviews", "ci-wait", "--target", "2:" + pins[0].HeadSHA + ":" + pins[0].BaseSHA, "--target", "4:" + pins[1].HeadSHA, "--wait-seconds", "0"}
	var cliResult client.CIWatchOutput
	if code := cli.RunContext(t.Context(), cliArgs, nil, &out, &diag); code != cli.ExitSuccess || diag.Len() != 0 || json.Unmarshal(out.Bytes(), &cliResult) != nil || !reflect.DeepEqual(result, cliResult) {
		t.Fatal("CLI route/result differs from registered MCP", code, out.String(), diag.String())
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = session.CallTool(ctx, &mcp.CallToolParams{Name: "wait_for_ci", Arguments: map[string]any{"targets": pins, "wait_seconds": 900}})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("long CI wait refused before HTTP")
	}
	readCtx, stopRead := context.WithTimeout(t.Context(), time.Second)
	value, err := session.CallTool(readCtx, &mcp.CallToolParams{Name: "context"})
	stopRead()
	if err != nil || value.IsError {
		t.Fatal("quiet CI wait blocked concurrent read", err)
	}
	select {
	case <-done:
		t.Fatal("pending CI wait exposed an intermediate result")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SDK wait ignored cancellation")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cancelled HTTP wait stayed open")
	}
	if posts.Load() != 3 {
		t.Fatal("CI read retried", posts.Load())
	}
}

func TestCIWatchSDKInvalidBeforeCredentials(t *testing.T) {
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer service.Close()
	identity := localContext()
	identity.ServerURL = service.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"chosen": identity}, "missing-credential"), "chosen"), "")
	for _, args := range []map[string]any{{}, {"targets": []client.CIWatchTarget{{PullNumber: 2, HeadSHA: "short"}}}, {"targets": []client.CIWatchTarget{{PullNumber: 2, HeadSHA: strings.Repeat("a", 40)}}, "wait_seconds": 901}} {
		diagnosticResult(t, call(t, session, "wait_for_ci", args), "invalid_arguments", false)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid CI watch reached credentials or HTTP")
	}
}
