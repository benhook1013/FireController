package client_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestQueueRestoreReorderRoutesAndSharedReceipts(t *testing.T) {
	for _, test := range []struct {
		action   string
		input    client.ReviewQueueChange
		body     string
		state    string
		revision int64
		invalid  bool
	}{
		{"restore", client.ReviewQueueChange{ExpectedConfigurationRevision: 3, PullNumber: 42}, `{"expected_configuration_revision":3,"pull_number":42}`, "configured", 4, false},
		{"restore", client.ReviewQueueChange{ExpectedConfigurationRevision: 3, PullNumber: 42, Position: "last"}, `{"expected_configuration_revision":3,"pull_number":42,"position":"last"}`, "configured", 4, false},
		{"reorder", client.ReviewQueueChange{ExpectedConfigurationRevision: 3, Order: []int64{42, 17}}, `{"expected_configuration_revision":3,"order":[42,17]}`, "configured", 4, false},
		{"reorder", client.ReviewQueueChange{ExpectedConfigurationRevision: 3, Order: []int64{42, 17}}, `{"expected_configuration_revision":3,"order":[42,17]}`, "unchanged", 3, false},
		{"restore", client.ReviewQueueChange{ExpectedConfigurationRevision: 3, PullNumber: 42}, `{"expected_configuration_revision":3,"pull_number":42}`, "unchanged", 3, true},
		{"restore", client.ReviewQueueChange{ExpectedConfigurationRevision: 3, PullNumber: 42}, `{"expected_configuration_revision":3,"pull_number":42}`, "configured", 3, true},
		{"reorder", client.ReviewQueueChange{ExpectedConfigurationRevision: 3, Order: []int64{42, 17}}, `{"expected_configuration_revision":3,"order":[42,17]}`, "configured", 5, true},
		{"reorder", client.ReviewQueueChange{ExpectedConfigurationRevision: 3, Order: []int64{42, 17}}, `{"expected_configuration_revision":3,"order":[42,17]}`, "unchanged", 4, true},
		{"reorder", client.ReviewQueueChange{ExpectedConfigurationRevision: 3, Order: []int64{42, 17}}, `{"expected_configuration_revision":3,"order":[42,17]}`, "queued", 4, true},
	} {
		t.Run(test.action+"/"+test.state, func(t *testing.T) {
			mutations := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/instance") {
					handshake(w)
					return
				}
				mutations++
				body, err := io.ReadAll(r.Body)
				if err != nil || r.Method != http.MethodPost || r.URL.Path != "/api/v1/projects/project-one/reviews/execution/queue/"+test.action || string(body) != test.body || r.Header.Get("Idempotency-Key") != "queue-key" || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" {
					t.Error("queue route/body/binding drift", r.Method, r.URL.Path, string(body), err)
				}
				write(w, http.StatusCreated, map[string]any{"id": "project-one", "revision": test.revision, "state": test.state})
			}))
			defer server.Close()
			out, err := newClient(t, server).ChangeReviewQueue(t.Context(), test.action, "queue-key", test.input)
			if mutations != 1 {
				t.Fatal("mutation was not sent exactly once", mutations)
			}
			if test.invalid {
				var diagnostic *client.Error
				if !errors.As(err, &diagnostic) || diagnostic.Code != "invalid_response" || !diagnostic.OutcomeUnknown {
					t.Fatal("invalid receipt lost exact-key recovery", out, err)
				}
			} else if err != nil || out.ID != "project-one" || out.Revision != test.revision || out.State != test.state {
				t.Fatal(out, err)
			}
		})
	}
}
