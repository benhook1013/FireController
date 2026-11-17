package client_test

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
)

func samplePolicyAssessment() client.ReviewPolicyAssessment {
	return client.ReviewPolicyAssessment{ProjectID: "project-one", EvidenceID: controlEvidenceID, Anchor: sampleControlEvidence().Anchor, Source: "captured_comparison", PolicyRevision: 2, EvaluatedAt: "2026-10-16T00:00:00Z", State: "held", Channels: []client.ReviewPolicyAssessmentChannel{{Channel: "cli", Covered: true}, {Channel: "hosted", Pending: true}}}
}

func TestReviewPolicyAssessmentBoundReadAndConsistentStates(t *testing.T) {
	var calls atomic.Int64
	var response atomic.Value
	response.Store(samplePolicyAssessment())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-FC-Instance") != "instance-one" || r.Method != "GET" || r.URL.RawQuery != "" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("assessment changed transport authority or requested a mutation/refresh")
		}
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		if r.URL.Path != "/api/v1/projects/project-one/reviews/evidence/"+controlEvidenceID+"/policy" || r.Header.Get("X-FC-Worker") != "worker-one" {
			t.Error("assessment lost project/evidence/worker binding")
		}
		write(w, 200, response.Load())
	}))
	defer srv.Close()
	c := newClient(t, srv)
	for _, state := range []string{"held", "sufficient", "unconfigured"} {
		value := samplePolicyAssessment()
		value.State = state
		if state == "sufficient" {
			value.Channels[1].Covered = true
			value.Channels[1].Pending = false
		}
		if state == "unconfigured" {
			value.PolicyRevision = 0
			value.Channels = []client.ReviewPolicyAssessmentChannel{}
		}
		response.Store(value)
		got, err := c.ReviewPolicyAssessment(t.Context(), controlEvidenceID)
		if err != nil || got.State != state || got.Source != "captured_comparison" || got.PolicyRevision != value.PolicyRevision || got.Anchor != value.Anchor {
			t.Fatal("assessment lost explicit comparison/state", got, err)
		}
	}
	// Closed current coverage does not erase another pending source obligation.
	pendingDominates := samplePolicyAssessment()
	pendingDominates.Channels[1].Covered = true
	response.Store(pendingDominates)
	if value, err := c.ReviewPolicyAssessment(t.Context(), controlEvidenceID); err != nil || value.State != "held" || !value.Channels[1].Covered || !value.Channels[1].Pending {
		t.Fatal("pending obligation was flattened into a passing channel", value, err)
	}
	before := calls.Load()
	for _, id := range []string{"", "../elsewhere", "review_run_" + strings.Repeat("a", 32), "review_evidence_" + strings.Repeat("A", 32)} {
		_, err := c.ReviewPolicyAssessment(t.Context(), id)
		checkError(t, err, "validation_failed")
	}
	if calls.Load() != before {
		t.Fatal("invalid assessment identifier reached HTTP")
	}
}

func TestReviewPolicyAssessmentRejectsMalformedBoundedResponses(t *testing.T) {
	valid, _ := json.Marshal(samplePolicyAssessment())
	var response atomic.Value
	response.Store(string(valid))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response.Load().(string)))
	}))
	defer srv.Close()
	c := newClient(t, srv)
	changed := func(edit func(map[string]any)) string {
		var value map[string]any
		if err := json.Unmarshal(valid, &value); err != nil {
			t.Fatal(err)
		}
		edit(value)
		out, _ := json.Marshal(value)
		return string(out)
	}
	cases := map[string]string{
		"foreign project":              changed(func(v map[string]any) { v["project_id"] = "another-project" }),
		"foreign evidence":             changed(func(v map[string]any) { v["evidence_id"] = "review_evidence_" + strings.Repeat("b", 32) }),
		"false current source":         changed(func(v map[string]any) { v["source"] = "current_source" }),
		"bad time":                     changed(func(v map[string]any) { v["evaluated_at"] = "not-a-time" }),
		"invalid anchor":               changed(func(v map[string]any) { v["anchor"].(map[string]any)["candidate_sha"] = strings.Repeat("A", 40) }),
		"missing explicit anchor zero": changed(func(v map[string]any) { delete(v["anchor"].(map[string]any), "parent_pull_number") }),
		"null anchor zero":             changed(func(v map[string]any) { v["anchor"].(map[string]any)["parent_pull_number"] = nil }),
		"duplicate channel":            changed(func(v map[string]any) { v["channels"].([]any)[1].(map[string]any)["channel"] = "cli" }),
		"unknown channel":              changed(func(v map[string]any) { v["channels"].([]any)[1].(map[string]any)["channel"] = "manual" }),
		"too many channels": changed(func(v map[string]any) {
			v["channels"] = append(v["channels"].([]any), map[string]any{"channel": "cli", "covered": true, "pending": false})
		}),
		"null channels":                  changed(func(v map[string]any) { v["channels"] = nil }),
		"configured empty":               changed(func(v map[string]any) { v["channels"] = []any{} }),
		"configured zero revision":       changed(func(v map[string]any) { v["policy_revision"] = 0 }),
		"unconfigured positive revision": changed(func(v map[string]any) { v["state"] = "unconfigured"; v["channels"] = []any{} }),
		"unconfigured nonempty":          changed(func(v map[string]any) { v["state"] = "unconfigured"; v["policy_revision"] = 0 }),
		"sufficient despite pending":     changed(func(v map[string]any) { v["state"] = "sufficient" }),
		"held despite all covered": changed(func(v map[string]any) {
			v["channels"].([]any)[1] = map[string]any{"channel": "hosted", "covered": true, "pending": false}
		}),
		"null false":      changed(func(v map[string]any) { v["channels"].([]any)[0].(map[string]any)["pending"] = nil }),
		"missing false":   changed(func(v map[string]any) { delete(v["channels"].([]any)[0].(map[string]any), "pending") }),
		"string false":    changed(func(v map[string]any) { v["channels"].([]any)[0].(map[string]any)["pending"] = "false" }),
		"unknown field":   changed(func(v map[string]any) { v["ready"] = true }),
		"unknown nested":  changed(func(v map[string]any) { v["channels"].([]any)[0].(map[string]any)["ready"] = true }),
		"case alias":      strings.Replace(string(valid), `"state":`, `"State":`, 1),
		"duplicate key":   strings.Replace(string(valid), `"state":`, `"state":"held","state":`, 1),
		"invalid Unicode": strings.Replace(string(valid), `"held"`, `"\ud800"`, 1),
		"over16KiB":       string(valid) + strings.Repeat(" ", 16<<10),
		"null root":       "null",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			response.Store(body)
			_, err := c.ReviewPolicyAssessment(t.Context(), controlEvidenceID)
			checkError(t, err, "invalid_response")
		})
	}
}

func TestReviewPolicyAssessmentDeadlineAndCancellationCloseRead(t *testing.T) {
	entered, closed := make(chan struct{}, 1), make(chan struct{}, 1)
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		entered <- struct{}{}
		<-r.Context().Done()
		closed <- struct{}{}
	}))
	defer srv.Close()
	c := newClient(t, srv)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.ReviewPolicyAssessment(ctx, controlEvidenceID)
	checkError(t, err, "deadline_exceeded")
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("deadline left HTTP read open")
	}
	if time.Since(start) > time.Second || calls.Load() != 1 {
		t.Fatal("read retried or exceeded caller budget")
	}
	cancelled, stop := context.WithCancel(t.Context())
	stop()
	_, err = c.ReviewPolicyAssessment(cancelled, controlEvidenceID)
	checkError(t, err, "canceled")
	if calls.Load() != 1 {
		t.Fatal("cancelled read reached HTTP")
	}
}
