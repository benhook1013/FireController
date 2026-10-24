package mcpadapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestReviewPolicyAssessmentMCPDiscoveryAndInvalidArgumentsAreLocal(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	binding := localContext()
	binding.ServerURL = service.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"alpha": binding}, "absent-private-credential"), "alpha")
	listed, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range listed.Tools {
		if tool.Name != "review_policy_assess" {
			continue
		}
		found = true
		var schema struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
			Additional bool           `json:"additionalProperties"`
		}
		raw, _ := json.Marshal(tool.InputSchema)
		if json.Unmarshal(raw, &schema) != nil || schema.Additional || len(schema.Properties) != 1 || schema.Properties["evidence_id"] == nil || strings.Join(schema.Required, ",") != "evidence_id" {
			t.Fatal("assessment schema permits missing identity or selectors")
		}
		a := tool.Annotations
		if a == nil || !a.ReadOnlyHint || !a.IdempotentHint || a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint || !strings.Contains(tool.Description, "completed PR/channel review progress") || !strings.Contains(tool.Description, "later Git changes preserve progress") || !strings.Contains(tool.Description, "No provider refresh") {
			t.Fatal("assessment effects/scope description incorrect")
		}
	}
	if !found {
		t.Fatal("assessment missing from discovery")
	}
	id := "review_evidence_" + strings.Repeat("a", 32)
	for _, raw := range []string{
		`null`, `[]`, `{}`, `{"evidence_id":null}`, `{"evidence_id":7}`, `{"evidence_id":""}`,
		`{"evidence_id":"../private"}`, `{"evidence_id":"review_run_` + strings.Repeat("a", 32) + `"}`,
		`{"evidence_id":"review_evidence_` + strings.Repeat("A", 32) + `"}`,
		`{"evidence_id":"` + id + `","extra":true}`, `{"evidence_id":"` + id + `","profile":"other"}`,
		`{"evidence_id":"` + id + `","worker_id":"other"}`, `{"evidence_id":"` + id + `","refresh":true}`,
		`{"Evidence_id":"` + id + `"}`, `{"evidence_id":"` + id + `","evidence_id":"` + id + `"}`,
		`{"evidence_id":"\ud800"}`, `{"evidence_id":"bad\u0000"}`,
	} {
		diagnosticResult(t, call(t, session, "review_policy_assess", json.RawMessage(raw)), "invalid_arguments", false)
	}
	if requests.Load() != 0 {
		t.Fatal("discovery or invalid arguments loaded missing credential/contacted HTTP")
	}
}

func TestReviewPolicyAssessmentMCPUsesActualPublicReadAndProfile(t *testing.T) {
	f, handler, evidenceID := reviewControlsFixture(t)
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "GET" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-FC-Instance") != f.store.InstanceID() {
			t.Error("assessment changed bound read authority")
		}
		if r.URL.Path != "/api/v1/instance" && (r.Header.Get("X-FC-Worker") != f.sender || r.URL.Path != "/api/v1/projects/"+f.project+"/reviews/evidence/"+evidenceID+"/policy") {
			t.Error("assessment changed project/worker/evidence selection")
		}
		handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, service.URL), "chosen")
	counts := func() [2]int {
		var result [2]int
		for i, table := range []string{"events", "receipts"} {
			if err := f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&result[i]); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	before, providers := counts(), f.providers.Load()
	var result client.ReviewPolicyAssessment
	decodeResult(t, call(t, session, "review_policy_assess", map[string]any{"evidence_id": evidenceID}), &result)
	if result.ProjectID != f.project || result.EvidenceID != evidenceID || result.Source != "captured_comparison" || result.State != "held" || result.PolicyRevision != 1 || len(result.Channels) != 2 || !result.Channels[0].Pending || result.Channels[0].Channel != "cli" || result.Channels[1].Channel != "hosted" || result.Channels[1].Covered {
		t.Fatal("actual assessment lost pending channel facts", result)
	}
	if requests.Load() != 2 || counts() != before || f.providers.Load() != providers {
		t.Fatal("assessment performed extra reads, provider work or writes")
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "PRIVATE_FINDING_TEXT") || strings.Contains(string(encoded), "merge_ready") {
		t.Fatal("assessment expanded source or invented readiness")
	}
}

func TestReviewPolicyAssessmentMCPCancellationReleasesOwnedHTTP(t *testing.T) {
	f, _, evidenceID := reviewControlsFixture(t)
	entered, closed := make(chan struct{}, 1), make(chan struct{}, 1)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		entered <- struct{}{}
		<-r.Context().Done()
		closed <- struct{}{}
	}))
	defer service.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, service.URL), "chosen")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "review_policy_assess", Arguments: map[string]any{"evidence_id": evidenceID}})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("assessment did not enter bounded HTTP read")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SDK cancellation did not complete")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("SDK cancellation left HTTP read open")
	}
	var resumed profile.Context
	decodeResult(t, call(t, session, "context", nil), &resumed)
	if resumed.ProjectID != f.project || resumed.WorkerID != f.sender {
		t.Fatal("cancellation broke the fixed session binding")
	}
}
