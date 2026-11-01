package mcpadapter_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestResultWaitRestartCompletesOriginalInvocationThroughSDKAndCLI(t *testing.T) {
	identity := localContext()
	id := "review_operation_" + strings.Repeat("a", 32)
	status := client.ReviewOperationStatus{ID: id, PullNumber: 2, Channel: "hosted", State: "completed", Revision: 4, Stage: "evidence_ready", AttemptID: "review_attempt_" + strings.Repeat("b", 32), EvidenceID: "review_evidence_" + strings.Repeat("c", 32), UpdatedAt: "2026-10-24T00:00:00Z", NextAction: "read_evidence"}
	pins := []client.CIWatchTarget{{PullNumber: 2, HeadSHA: strings.Repeat("d", 40), BaseSHA: strings.Repeat("e", 40)}, {PullNumber: 7, HeadSHA: strings.Repeat("f", 40)}}
	ci := client.CIWatchResult{ProjectID: identity.ProjectID, State: "passed", Targets: []client.CIWatchTargetResult{}}
	for index, pin := range pins {
		ci.Targets = append(ci.Targets, client.CIWatchTargetResult{PullNumber: pin.PullNumber, HeadSHA: pin.HeadSHA, BaseSHA: pin.BaseSHA, State: "passed", Reason: "ci_satisfied", BaseRef: "feature", GateRef: "main", ObservedHeadSHA: pin.HeadSHA, ObservedBaseSHA: strings.Repeat("e", 40), ObservedAt: status.UpdatedAt, CICommitSHA: pin.HeadSHA, Selection: "head", Required: 1, Satisfied: 1, Checks: []client.ReviewCIContext{{Context: "required-build", State: "satisfied", CheckRun: &client.ReviewCICheckRun{ID: int64(index + 81), Status: "completed", Conclusion: "success"}}}})
	}
	counts := map[string]int{}
	firstWait := map[string]int{}
	var ciBody []byte
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			jsonResponse(w, map[string]string{"instance_id": identity.InstanceID})
			return
		}
		operation := r.Method == "GET" && r.URL.Path == "/api/v1/projects/"+identity.ProjectID+"/reviews/operations/"+id+"/status"
		watch := r.Method == "POST" && r.URL.Path == "/api/v1/projects/"+identity.ProjectID+"/reviews/ci/watch"
		if !operation && !watch || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Instance") != identity.InstanceID || r.Header.Get("X-FC-Worker") != identity.WorkerID || r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("reattachment changed bound identity or submitted work")
		}
		if watch {
			body, _ := io.ReadAll(r.Body)
			if ciBody == nil {
				ciBody = bytes.Clone(body)
			} else if !bytes.Equal(ciBody, body) {
				t.Error("original CI body changed")
			}
		}
		counts[r.URL.Path]++
		seconds, err := strconv.Atoi(strings.TrimPrefix(r.Header.Get("Prefer"), "wait="))
		if err != nil {
			t.Error(err)
		}
		if counts[r.URL.Path]%2 == 1 {
			if seconds <= 0 || seconds > 10 {
				t.Error("initial finite wait changed", seconds)
			}
			firstWait[r.URL.Path] = seconds
			w.Header().Set("X-Request-ID", "request-controlled-restart")
			core.WriteError(w, &core.Error{Code: "overloaded", Status: 503, Message: "shutdown", Details: map[string]any{"reason": "result_wait_lifetime"}})
			return
		}
		if seconds < 0 || seconds >= firstWait[r.URL.Path] {
			t.Error("reconnect reset wait budget", seconds, firstWait[r.URL.Path])
		}
		if operation {
			jsonResponse(w, status)
		} else {
			jsonResponse(w, ci)
		}
	}))
	defer service.Close()
	identity.ServerURL = service.URL
	path := profiles(t, map[string]profile.Context{"chosen": identity}, credential(t))
	session := connect(t, adapter(t, path, "chosen"), "")
	var got client.ReviewOperationStatus
	decodeResult(t, call(t, session, "review_operation_read", map[string]any{"operation_id": id, "wait_seconds": 10}), &got)
	if got.ID != id || got.State != "completed" || got.EvidenceID != status.EvidenceID {
		t.Fatal("one SDK invocation lost original completion", got)
	}
	var gotCI client.CIWatchResult
	decodeResult(t, call(t, session, "wait_for_ci", map[string]any{"targets": pins, "wait_seconds": 10}), &gotCI)
	if !reflect.DeepEqual(gotCI, ci) {
		t.Fatal("one SDK invocation lost original CI pins/result", gotCI)
	}
	var out, diag bytes.Buffer
	if code := cli.RunContext(t.Context(), []string{"--profiles-file", path, "--profile", "chosen", "reviews", "operation", id, "--wait-seconds", "10"}, nil, &out, &diag); code != 0 || diag.Len() != 0 || json.Unmarshal(out.Bytes(), &got) != nil || got.ID != id || got.State != "completed" {
		t.Fatal("CLI invocation did not survive restart", code, out.String(), diag.String())
	}
	out.Reset()
	diag.Reset()
	if code := cli.RunContext(t.Context(), []string{"--profiles-file", path, "--profile", "chosen", "reviews", "ci-wait", "--target", "2:" + pins[0].HeadSHA + ":" + pins[0].BaseSHA, "--target", "7:" + pins[1].HeadSHA, "--wait-seconds", "10"}, nil, &out, &diag); code != 0 || diag.Len() != 0 || json.Unmarshal(out.Bytes(), &gotCI) != nil || !reflect.DeepEqual(gotCI, ci) {
		t.Fatal("CLI CI invocation did not survive restart", code, out.String(), diag.String())
	}
	if len(counts) != 2 {
		t.Fatal("extra enumeration/identity/mutation calls", counts)
	}
	for _, count := range counts {
		if count != 4 {
			t.Fatal("restart repeated or lost original invocation", counts)
		}
	}
}
