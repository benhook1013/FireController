package client_test

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/client"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMissingParentSourceRefusalWireNegotiatesV2(t *testing.T) {
	full := sampleReviewOperation()
	full.State, full.Reason, full.AttemptID = "held", "parent_unavailable", "review_attempt_"+strings.Repeat("b", 32)
	full.SourceRefusal = &client.ReviewSourceRefusal{PullNumber: 4, BaseRef: "codex/former-parent"}
	compact := client.ReviewOperationStatus{ID: full.ID, PullNumber: full.PullNumber, Channel: full.Channel, State: full.State, Reason: full.Reason, AttemptID: full.AttemptID, Stage: "held", NextAction: "read_diagnostics", Revision: full.Revision, UpdatedAt: full.UpdatedAt, SourceRefusal: full.SourceRefusal}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		if r.Header.Get("X-FC-Source-Refusal") != "v2" {
			t.Error("client did not negotiate missing-parent variant")
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/status") {
			json.NewEncoder(w).Encode(compact)
		} else {
			json.NewEncoder(w).Encode(full)
		}
	}))
	defer s.Close()
	c := newClient(t, s)
	a, err := c.ReviewOperation(t.Context(), full.ID)
	if err != nil || a.SourceRefusal == nil || a.SourceRefusal.BaseRef != full.SourceRefusal.BaseRef {
		t.Fatal(a, err)
	}
	b, err := c.ReviewOperationStatus(t.Context(), full.ID)
	if err != nil || b.SourceRefusal == nil || b.SourceRefusal.PullNumber != 4 {
		t.Fatal(b, err)
	}
}
