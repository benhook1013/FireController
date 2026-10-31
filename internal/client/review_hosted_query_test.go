package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestHostedQueryDiagnosticsClientNegotiationAndValidation(t *testing.T) {
	for _, mode := range []string{"valid", "unconfirmed", "legacy", "foreign URL", "wrong PR", "private query", "partial capture", "private path", "wrong channel", "wrong state"} {
		t.Run(mode, func(t *testing.T) {
			value := sampleReviewOperation()
			value.Channel, value.State, value.Reason, value.AttemptID = "hosted", "held", "hosted_allowance_unproven", "review_attempt_"+strings.Repeat("b", 32)
			q := &client.ReviewHostedQueryDiagnostic{QueryID: "review_hosted_query_" + strings.Repeat("c", 32), QueryCommentID: 123, QueryURL: "https://github.com/owner/repo/pull/2#issuecomment-123", ObservationID: "review_hosted_observation_" + strings.Repeat("d", 32), CaptureID: "review_capture_" + strings.Repeat("e", 32), CapturedAt: value.UpdatedAt}
			value.PullNumber, value.HostedQuery = 2, q
			switch mode {
			case "unconfirmed":
				q.QueryCommentID, q.QueryURL = 0, ""
			case "legacy":
				value.HostedQuery = nil
			case "foreign URL":
				q.QueryURL = "https://private.example/pull/2#issuecomment-123"
			case "wrong PR":
				q.QueryURL = "https://github.com/owner/repo/pull/3#issuecomment-123"
			case "private query":
				q.QueryURL = "https://github.com/owner/repo/pull/2?token=PRIVATE#issuecomment-123"
			case "partial capture":
				q.CaptureID = ""
			case "private path":
				q.QueryID = "/private/capture.json"
			case "wrong channel":
				value.Channel = "cli"
			case "wrong state":
				value.State = "completed"
			}
			reads := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				reads++
				if r.Method != "GET" || r.Header.Get("X-FC-Hosted-Query") != "1" || r.Header.Get("Idempotency-Key") != "" {
					t.Error("diagnostic changed explicit read negotiation")
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer s.Close()
			got, err := newClient(t, s).ReviewOperation(t.Context(), value.ID)
			valid := mode == "valid" || mode == "unconfirmed" || mode == "legacy"
			if (err == nil) != valid || reads != 1 {
				t.Fatal("invalid original diagnostic accepted or read retried", mode, err, reads)
			}
			if valid {
				if !reflect.DeepEqual(got.HostedQuery, value.HostedQuery) {
					t.Fatal("original query facts changed")
				}
				output := client.AgentReviewOperation(got)
				if output.Diagnostic == nil || strings.Contains(output.Diagnostic.NextAction, "maintainer") {
					t.Fatal("diagnostic still needs a controller owner")
				}
			}
		})
	}
}
