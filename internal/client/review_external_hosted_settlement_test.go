package client_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestExternalHostedSettlementStrictInput(t *testing.T) {
	run := `{"run_id":"review_run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","findings":[]}`
	clean := `{"count_round":false,"runs":[` + run + `]}`
	value, err := client.DecodeExternalHostedSettlementRequest([]byte(clean))
	if err != nil || value.CountRound || len(value.Runs) != 1 || value.Runs[0].Findings == nil {
		t.Fatal("explicit false and empty findings did not survive decoding", err)
	}
	finding := `{"finding_id":"provider:17","disposition":"accepted","reason":"Verified","fix_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","proof_note":"Published fix"}`
	accepted := strings.Replace(clean, `"findings":[]`, `"findings":[`+finding+`]`, 1)
	if _, err = client.DecodeExternalHostedSettlementRequest([]byte(accepted)); err != nil {
		t.Fatal("complete accepted decision rejected", err)
	}
	for name, raw := range map[string]string{
		"omitted choice":    strings.Replace(clean, `"count_round":false,`, "", 1),
		"null choice":       strings.Replace(clean, "false", "null", 1),
		"omitted findings":  strings.Replace(clean, `,"findings":[]`, "", 1),
		"null findings":     strings.Replace(clean, `"findings":[]`, `"findings":null`, 1),
		"duplicate run":     strings.Replace(clean, run, run+","+run, 1),
		"duplicate finding": strings.Replace(accepted, finding, finding+","+finding, 1),
		"absent proof":      strings.Replace(accepted, `,"proof_note":"Published fix"`, "", 1),
		"null fix":          strings.Replace(accepted, `"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`, "null", 1),
		"rejected with fix": strings.Replace(accepted, `"accepted"`, `"rejected"`, 1),
		"unknown field":     strings.Replace(accepted, `"reason":"Verified"`, `"reason":"Verified","extra":0`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := client.DecodeExternalHostedSettlementRequest([]byte(raw)); err == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
}

func TestExternalHostedSettlementReceiptRecoveryKeepsExactRequest(t *testing.T) {
	var posts atomic.Int64
	var original atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/external-hosted/2/settlement" ||
			r.Header.Get("Idempotency-Key") != "settle-once" || r.Header.Get("X-FC-Worker") != "worker-one" ||
			r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("settlement lost bound identity or key")
		}
		raw, err := io.ReadAll(r.Body)
		value, decodeErr := client.DecodeExternalHostedSettlementRequest(raw)
		if err != nil || decodeErr != nil || value.CountRound || len(value.Runs) != 1 || len(value.Runs[0].Findings) != 0 {
			t.Error("settlement did not preserve explicit false and empty findings", err, decodeErr)
		}
		receipt := map[string]any{"id": "review_hosted_batch_" + strings.Repeat("c", 32), "state": "external_hosted_settled", "runs": 1, "round_counted": false}
		if posts.Add(1) == 1 {
			original.Store(string(raw))
			delete(receipt, "round_counted")
		} else if original.Load() != string(raw) {
			t.Error("exact replay changed request bytes")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(receipt)
	}))
	defer server.Close()
	c := newClient(t, server)
	defer c.Close()
	value := client.ExternalHostedSettlementRequest{Runs: []client.ExternalHostedSettlementRun{{RunID: "review_run_" + strings.Repeat("a", 32), Findings: []client.ExternalHostedSettlementFinding{}}}}
	_, err := c.SettleExternalHosted(t.Context(), "settle-once", 2, value)
	var failure *client.Error
	if !errors.As(err, &failure) || failure.Code != "invalid_response" || !failure.OutcomeUnknown || posts.Load() != 1 {
		t.Fatal("malformed receipt did not preserve an uncertain mutation for explicit replay", err, posts.Load())
	}
	receipt, err := c.SettleExternalHosted(t.Context(), "settle-once", 2, value)
	if err != nil || receipt.RoundCounted || receipt.Runs != 1 || posts.Load() != 2 {
		t.Fatal("same-key replay did not recover the settlement", receipt, err, posts.Load())
	}
}

func TestExternalHostedSettlementPendingReviewGuidesBoundRecovery(t *testing.T) {
	runID := "review_run_" + strings.Repeat("b", 32)
	var bad atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		ids := []string{runID}
		if bad.Load() {
			ids = append(ids, "untrusted provider prose")
		}
		write(w, http.StatusConflict, map[string]any{
			"request_id": "request-one",
			"error": map[string]any{
				"code":    "external_hosted_pending",
				"message": "Untrusted server prose",
				"details": map[string]any{"pull_number": 2, "run_ids": ids, "has_more": false},
			},
		})
	}))
	defer server.Close()
	c := newClient(t, server)
	defer c.Close()
	request := client.ReviewRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "hosted", HeadSHA: strings.Repeat("a", 40)}
	_, err := c.RequestReview(t.Context(), "new-review", request, 0)
	var failure *client.Error
	if !errors.As(err, &failure) || failure.Code != "external_hosted_pending" || failure.OutcomeUnknown ||
		!strings.Contains(failure.Action, "reviews settle-hosted") || strings.Contains(failure.Message, "Untrusted") {
		t.Fatal("review refusal lost the actual recovery", err)
	}
	details, _ := json.Marshal(failure.Details)
	if !strings.Contains(string(details), runID) {
		t.Fatal("review refusal lost its validated run identity", string(details))
	}
	bad.Store(true)
	_, err = c.RequestReview(t.Context(), "new-review", request, 0)
	if !errors.As(err, &failure) || failure.Details != nil {
		t.Fatal("unsafe provider detail crossed the diagnostic boundary", err)
	}
	status := client.AgentReviewOperationStatus(client.ReviewOperationStatus{Channel: "hosted", State: "held", Reason: "external_hosted_pending", NextAction: "read_diagnostics"})
	if status.NextAction != "settle_external_hosted" || status.Diagnostic == nil || !strings.Contains(status.Diagnostic.NextAction, "reviews runs PR") {
		t.Fatal("stored hold lost settlement guidance", status)
	}
}
