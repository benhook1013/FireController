package client_test

import (
	"context"
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

const decisionBatchJSON = `{"expected_revision":0,"items":[{"finding":0,"disposition":"accepted","reason":"  Exact selected reason\n"},{"finding":2,"disposition":"rejected","reason":"Explicit rejection","expected_route_revision":3}]}`

func TestReviewBatchClientStrictExplicitWire(t *testing.T) {
	valid, err := client.DecodeReviewDecisionBatchRequest([]byte(decisionBatchJSON))
	if err != nil || valid.Items[0].Finding != 0 || valid.Items[0].Reason != "  Exact selected reason\n" {
		t.Fatal("explicit batch changed", err)
	}
	for _, body := range []string{
		`null`, `[]`, `{"items":[]}`, `{"expected_revision":0,"items":null}`, `{"expected_revision":0,"items":[]}`,
		`{"expected_revision":0,"items":[null]}`,
		strings.Replace(decisionBatchJSON, `"finding":0,`, "", 1), strings.Replace(decisionBatchJSON, `"finding":0`, `"finding":null`, 1),
		strings.Replace(decisionBatchJSON, `"finding":2`, `"finding":0`, 1), strings.Replace(decisionBatchJSON, `"finding":2`, `"finding":100`, 1),
		strings.Replace(decisionBatchJSON, `"expected_route_revision":3`, `"expected_route_revision":null`, 1),
		strings.Replace(decisionBatchJSON, `"reason":"Explicit rejection"`, `"reason":"Explicit rejection","extra":true`, 1),
		strings.Replace(decisionBatchJSON, `"finding":0`, `"Finding":0`, 1), strings.Replace(decisionBatchJSON, `"finding":0`, `"finding":0,"finding":1`, 1),
		strings.Replace(decisionBatchJSON, `Exact selected reason`, `\ud800`, 1), strings.Repeat(" ", client.ReviewBatchMaxBytes) + decisionBatchJSON,
	} {
		if _, err := client.DecodeReviewDecisionBatchRequest([]byte(body)); err == nil {
			t.Fatal("malformed decision batch accepted")
		}
	}
	fix := `{"expected_revision":1,"items":[{"finding":0,"expected_sha":"","fix_sha":"` + strings.Repeat("a", 40) + `","proof_note":"Exact proof"}]}`
	if _, err := client.DecodeReviewFixBatchRequest([]byte(fix)); err != nil {
		t.Fatal("explicit empty expected SHA rejected", err)
	}
	for _, body := range []string{strings.Replace(fix, `"expected_sha":"",`, "", 1), strings.Replace(fix, `"expected_sha":""`, `"expected_sha":null`, 1), strings.Replace(fix, `"finding":0,`, "", 1), strings.Replace(fix, `"proof_note":"Exact proof"`, `"proof_note":" "`, 1)} {
		if _, err := client.DecodeReviewFixBatchRequest([]byte(body)); err == nil {
			t.Fatal("malformed fix batch accepted")
		}
	}
}

func TestReviewBatchClientBoundRequestCompactReceiptAndNoRetry(t *testing.T) {
	var calls atomic.Int64
	var bad atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.Method != "POST" || r.Header.Get("Idempotency-Key") != "exact-batch-key" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("batch lost mutation binding")
		}
		body, _ := io.ReadAll(r.Body)
		state := "adjudicated_batch"
		expected := int64(1)
		switch r.URL.Path {
		case "/api/v1/projects/project-one/reviews/evidence/" + controlEvidenceID + "/decisions/batch":
			value, err := client.DecodeReviewDecisionBatchRequest(body)
			if err != nil || len(value.Items) != 2 || value.Items[0].Reason != "  Exact selected reason\n" {
				t.Error("decision wire changed", err)
			}
		case "/api/v1/projects/project-one/reviews/evidence/" + controlEvidenceID + "/fixes/batch":
			state, expected = "fixes_recorded_batch", 2
			value, err := client.DecodeReviewFixBatchRequest(body)
			if err != nil || value.Items[0].ExpectedSHA != "" {
				t.Error("fix wire changed", err)
			}
		default:
			t.Error("incorrect batch route", r.URL.EscapedPath())
		}
		if bad.Load() {
			expected++
		}
		write(w, 200, client.ReviewControlReceipt{ID: controlEvidenceID, State: state, Revision: expected})
	}))
	defer server.Close()
	c := newClient(t, server)
	defer c.Close()
	decision, _ := client.DecodeReviewDecisionBatchRequest([]byte(decisionBatchJSON))
	r, err := c.DecideReviewFindings(context.Background(), "exact-batch-key", controlEvidenceID, decision)
	if err != nil || r.State != "adjudicated_batch" {
		t.Fatal(err)
	}
	f := client.ReviewFixBatchRequest{ExpectedRevision: 1, Items: []client.ReviewFixBatchItem{{Finding: 0, ExpectedSHA: "", FixSHA: strings.Repeat("a", 40), ProofNote: "Proof"}}}
	r, err = c.RecordReviewFixes(context.Background(), "exact-batch-key", controlEvidenceID, f)
	if err != nil || r.State != "fixes_recorded_batch" {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(r)
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	if len(fields) != 3 || len(raw) > 200 {
		t.Fatal("receipt expanded source")
	}
	invalid := decision
	invalid.Items = append([]client.ReviewDecisionBatchItem(nil), decision.Items...)
	invalid.Items[1].Finding = 0
	_, err = c.DecideReviewFindings(context.Background(), "exact-batch-key", controlEvidenceID, invalid)
	if err == nil || calls.Load() != 2 {
		t.Fatal("invalid batch reached HTTP")
	}
	bad.Store(true)
	_, err = c.DecideReviewFindings(context.Background(), "exact-batch-key", controlEvidenceID, decision)
	var failure *client.Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown || calls.Load() != 3 {
		t.Fatal("bad committed receipt lost outcome uncertainty/retried", err, calls.Load())
	}
}
