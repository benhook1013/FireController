package client_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewFixCorrectionDriverValidationWireAndExplicitReplay(t *testing.T) {
	request := client.ReviewFixCorrectionRequest{ExpectedRevision: 7, Finding: 0, ExpectedSHA: strings.Repeat("f", 40), Reason: "  PRIVATE_CORRECTION_REASON\n"}
	var posts atomic.Int64
	var drop atomic.Bool
	drop.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		posts.Add(1)
		var wire map[string]json.RawMessage
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/projects/project-one/reviews/evidence/"+controlEvidenceID+"/corrections" || r.URL.RawQuery != "" || r.Header.Get("Idempotency-Key") != "original-correction-key" || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Authorization") != "Bearer "+testToken || json.NewDecoder(r.Body).Decode(&wire) != nil {
			t.Error("correction lost its method, binding, key or strict body")
		}
		var reason string
		if len(wire) != 4 || string(wire["finding"]) != "0" || string(wire["expected_revision"]) != "7" || string(wire["expected_sha"]) != `"`+request.ExpectedSHA+`"` || json.Unmarshal(wire["reason"], &reason) != nil || reason != request.Reason {
			t.Error("correction changed explicit guards or exact reason")
		}
		if drop.Swap(false) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		write(w, http.StatusOK, client.ReviewControlReceipt{ID: controlEvidenceID, State: "adjudication_corrected", Revision: 8})
	}))
	defer server.Close()
	c := newClient(t, server)
	defer c.Close()
	for _, change := range []func(*client.ReviewFixCorrectionRequest){
		func(r *client.ReviewFixCorrectionRequest) { r.ExpectedRevision = -1 },
		func(r *client.ReviewFixCorrectionRequest) { r.ExpectedRevision = 1<<63 - 1 },
		func(r *client.ReviewFixCorrectionRequest) { r.Finding = -1 },
		func(r *client.ReviewFixCorrectionRequest) { r.Finding = 100 },
		func(r *client.ReviewFixCorrectionRequest) { r.ExpectedSHA = "" },
		func(r *client.ReviewFixCorrectionRequest) { r.ExpectedSHA = strings.Repeat("a", 39) },
		func(r *client.ReviewFixCorrectionRequest) { r.ExpectedSHA = strings.Repeat("A", 40) },
		func(r *client.ReviewFixCorrectionRequest) { r.Reason = " \n" },
		func(r *client.ReviewFixCorrectionRequest) { r.Reason = strings.Repeat("é", 1025) },
		func(r *client.ReviewFixCorrectionRequest) { r.Reason = "bad\x00" },
		func(r *client.ReviewFixCorrectionRequest) { r.Reason = "bad\xff" },
	} {
		bad := request
		change(&bad)
		_, err := c.CorrectReviewFindingFix(t.Context(), "original-correction-key", controlEvidenceID, bad)
		checkError(t, err, "validation_failed", request.Reason)
	}
	for _, binding := range [][2]string{{"", controlEvidenceID}, {" padded ", controlEvidenceID}, {"original-correction-key", "../other"}} {
		_, err := c.CorrectReviewFindingFix(t.Context(), binding[0], binding[1], request)
		checkError(t, err, "validation_failed")
	}
	if posts.Load() != 0 {
		t.Fatal("invalid correction reached HTTP")
	}
	_, err := c.CorrectReviewFindingFix(t.Context(), "original-correction-key", controlEvidenceID, request)
	var failure *client.Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown || posts.Load() != 1 || !strings.Contains(failure.Action, "same") {
		t.Fatal("lost response retried or lost exact-key recovery", err)
	}
	for range 2 {
		receipt, err := c.CorrectReviewFindingFix(t.Context(), "original-correction-key", controlEvidenceID, request)
		if err != nil || receipt != (client.ReviewControlReceipt{ID: controlEvidenceID, State: "adjudication_corrected", Revision: 8}) {
			t.Fatal("explicit replay rejected original revision/key receipt", receipt, err)
		}
	}
	if posts.Load() != 3 {
		t.Fatal("correction added unrequested calls")
	}
}

func TestReviewFixCorrectionDriverRejectsInvalidReceipts(t *testing.T) {
	for _, receipt := range []client.ReviewControlReceipt{
		{ID: controlEvidenceID, State: "fix_recorded", Revision: 8},
		{ID: controlEvidenceID, State: "adjudication_corrected", Revision: 7},
		{ID: controlEvidenceID, State: "adjudication_corrected", Revision: 9},
		{ID: "review_evidence_" + strings.Repeat("b", 32), State: "adjudication_corrected", Revision: 8},
	} {
		var posts atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/instance" {
				handshake(w)
				return
			}
			posts.Add(1)
			write(w, http.StatusOK, receipt)
		}))
		c := newClient(t, server)
		_, err := c.CorrectReviewFindingFix(t.Context(), "key", controlEvidenceID, client.ReviewFixCorrectionRequest{ExpectedRevision: 7, ExpectedSHA: strings.Repeat("f", 40), Reason: "Private reason"})
		var failure *client.Error
		if !errors.As(err, &failure) || failure.Code != "invalid_response" || !failure.OutcomeUnknown || posts.Load() != 1 {
			t.Fatal("invalid correction receipt was accepted or retried", err)
		}
		c.Close()
		server.Close()
	}
}

func TestReviewFixCorrectionDriverPreservesGuardAndKeyRefusals(t *testing.T) {
	for _, code := range []string{"review_conflict", "idempotency_conflict", "forbidden"} {
		t.Run(code, func(t *testing.T) {
			var posts atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				posts.Add(1)
				status := http.StatusConflict
				if code == "forbidden" {
					status = http.StatusForbidden
				}
				write(w, status, map[string]any{"error": map[string]any{"code": code, "message": "PRIVATE_SERVER_TEXT", "details": map[string]any{"reason": "adjudication_revision_conflict", "field": "expected_revision", "current_revision": 9}}, "request_id": "correction-request"})
			}))
			defer server.Close()
			c := newClient(t, server)
			defer c.Close()
			_, err := c.CorrectReviewFindingFix(t.Context(), "original-key", controlEvidenceID, client.ReviewFixCorrectionRequest{ExpectedRevision: 7, ExpectedSHA: strings.Repeat("f", 40), Reason: "PRIVATE_CORRECTION_REASON"})
			checkError(t, err, code, "PRIVATE_SERVER_TEXT", "PRIVATE_CORRECTION_REASON")
			var failure *client.Error
			if !errors.As(err, &failure) || failure.OutcomeUnknown || posts.Load() != 1 {
				t.Fatal("known correction refusal was retried or became uncertain", err)
			}
			if code == "review_conflict" {
				details, _ := failure.Details.(map[string]any)
				if details["current_revision"] != int64(9) || details["field"] != "expected_revision" {
					t.Fatal("correction lost its authoritative revision guard", failure)
				}
			}
		})
	}
}
