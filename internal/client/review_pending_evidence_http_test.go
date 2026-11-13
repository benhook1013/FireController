package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestPendingEvidenceHTTPReadRequestsOneExpandedLocalResult(t *testing.T) {
	sample := sampleReviewOperation()
	sample.State = "held"
	sample.Reason = "review_evidence_pending"
	sample.WorkerID = "worker-one"
	finding := int64(0)
	sample.PendingEvidence = &client.ReviewPendingEvidence{EvaluatedAt: sample.UpdatedAt, Items: []client.ReviewPendingEvidenceItem{{OperationID: sample.ID, EvidenceID: "review_evidence_" + strings.Repeat("b", 32), Finding: &finding, RouteID: "review_route_" + strings.Repeat("c", 32), TargetPullNumber: 42, Reason: "route_fix_pending"}}}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.Header.Get("X-FC-Review-Blockers") != "true" || r.Method != "GET" {
			t.Error("missing explicit expansion")
		}
		write(w, http.StatusOK, sample)
	}))
	defer server.Close()
	c := newClient(t, server)
	got, err := c.ReviewOperation(context.Background(), sample.ID)
	if err != nil || got.PendingEvidence == nil || len(got.PendingEvidence.Items) != 1 || calls.Load() != 1 {
		t.Fatal("expanded read failed or polled", got, err)
	}
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), "route_fix_pending") {
		t.Fatal("blocker lost")
	}
}
