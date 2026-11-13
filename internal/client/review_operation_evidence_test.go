package client_test

import (
	"github.com/benhook1013/FireController/internal/client"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFullOperationEvidenceUsesOneScopedReadAndValidatesHandle(t *testing.T) {
	for _, mode := range []string{"present", "absent", "invalid", "missing_attempt"} {
		t.Run(mode, func(t *testing.T) {
			op := sampleReviewOperation()
			op.State = "completed"
			op.AttemptID = "review_attempt_" + strings.Repeat("b", 32)
			if mode != "absent" {
				op.EvidenceID = "review_evidence_" + strings.Repeat("c", 32)
			}
			if mode == "invalid" {
				op.EvidenceID = "review_run_" + strings.Repeat("c", 32)
			}
			if mode == "missing_attempt" {
				op.AttemptID = ""
			}
			var reads atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				reads.Add(1)
				if r.Method != "GET" || r.URL.Path != "/api/v1/projects/project-one/reviews/operations/"+op.ID || r.Header.Get("X-FC-Operation-Evidence") != "true" || r.Header.Get("Idempotency-Key") != "" {
					t.Error("diagnostics changed its read boundary")
				}
				write(w, 200, op)
			}))
			defer s.Close()
			c, err := client.New(t.Context(), binding(t, s.URL))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			got, err := c.ReviewOperation(t.Context(), op.ID)
			if mode == "invalid" || mode == "missing_attempt" {
				checkError(t, err, "invalid_response")
			} else if err != nil || got.EvidenceID != op.EvidenceID {
				t.Fatal(got, err)
			}
			if reads.Load() != 1 {
				t.Fatal("diagnostics added client requests", reads.Load())
			}
		})
	}
}
