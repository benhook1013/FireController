package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestDeploymentDrainResponseExplainsQueueWithoutAnotherRead(t *testing.T) {
	sample := sampleReviewOperation()
	sample.State = "queued"
	sample.Reason = ""
	sample.AttemptID = ""
	sample.Revision = 1
	sample.WorkerID = "worker-one"
	for _, mode := range []string{"compact", "full", "receipt"} {
		t.Run(mode, func(t *testing.T) {
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				reads++
				w.Header().Set("X-FC-Deployment-Draining", "true")
				switch mode {
				case "compact":
					write(w, 200, client.ReviewOperationStatus{ID: sample.ID, PullNumber: sample.PullNumber, Channel: sample.Channel, State: "queued", Stage: "queued", NextAction: "wait", Revision: 1, UpdatedAt: sample.UpdatedAt})
				case "full":
					write(w, 200, sample)
				case "receipt":
					write(w, 202, client.ReviewRequestReceipt{ID: sample.ID, State: "queued", Revision: 1})
				}
			}))
			defer server.Close()
			c := newClient(t, server)
			var out any
			switch mode {
			case "compact":
				v, err := c.ReviewOperationStatus(context.Background(), sample.ID)
				if err != nil {
					t.Fatal(err)
				}
				out = client.AgentReviewOperationStatus(v)
			case "full":
				v, err := c.ReviewOperation(context.Background(), sample.ID)
				if err != nil {
					t.Fatal(err)
				}
				out = client.AgentReviewOperation(v)
			case "receipt":
				v, err := c.RequestReview(context.Background(), "drain-receipt", client.ReviewRequest{ExpectedConfigurationRevision: 1, PullNumber: sample.PullNumber, Channel: sample.Channel, HeadSHA: sample.HeadSHA}, 0)
				if err != nil {
					t.Fatal(err)
				}
				out = client.AgentReviewRequestResult(v)
			}
			raw, err := json.Marshal(out)
			if err != nil || reads != 1 || !strings.Contains(string(raw), "draining new dispatch") || strings.Contains(string(raw), "deploymentDraining") {
				t.Fatal("missing or unbounded queue context", string(raw), err, reads)
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(raw, &fields) != nil || string(fields["state"]) != `"queued"` || string(fields["id"]) != `"`+sample.ID+`"` || fields["diagnostic"] == nil {
				t.Fatal("changed receipt or operation identity", string(raw))
			}
		})
	}
}
