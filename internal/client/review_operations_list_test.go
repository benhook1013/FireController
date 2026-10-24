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

func operationListClientItem() client.ReviewOperationSummary {
	return client.ReviewOperationSummary{ID: "review_operation_" + strings.Repeat("a", 32), PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40), WorkerID: "other-worker", State: "queued", Revision: 1, CreatedAt: "2026-10-23T12:00:00Z", UpdatedAt: "2026-10-23T12:00:00Z"}
}

func TestOperationListClientBoundFiltersAndNoAutomaticContinuation(t *testing.T) {
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/api/v1/projects/project-one/reviews/operations" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-FC-Instance") != "instance-one" {
			t.Error("list binding changed")
		}
		if calls.Load() == 1 && r.URL.RawQuery != "limit=5&status=pending" {
			t.Error("list defaults changed", r.URL.RawQuery)
		}
		if calls.Load() == 2 && r.URL.RawQuery != "channel=cli&cursor=signed-cursor&limit=1&pull_number=2&status=all" {
			t.Error("list filters changed", r.URL.RawQuery)
		}
		write(w, 200, client.ReviewOperationsPage{ProjectID: "project-one", Items: []client.ReviewOperationSummary{operationListClientItem()}, HasMore: true, Cursor: "signed-next"})
	}))
	defer service.Close()
	c := newClient(t, service)
	page, err := c.ReviewOperations(t.Context(), client.ReviewOperationsOptions{})
	if err != nil || len(page.Items) != 1 || page.Items[0].WorkerID != "other-worker" || calls.Load() != 1 || !page.HasMore {
		t.Fatal(page, err, calls.Load())
	}
	_, err = c.ReviewOperations(t.Context(), client.ReviewOperationsOptions{PageOptions: client.PageOptions{Limit: 1, Cursor: "signed-cursor"}, PullNumber: 2, Channel: "cli", Status: "all"})
	if err != nil || calls.Load() != 2 {
		t.Fatal(err, calls.Load())
	}
	for _, bad := range []client.ReviewOperationsOptions{{PullNumber: -1}, {PullNumber: 2147483648}, {Channel: "other"}, {Status: "completed"}, {PageOptions: client.PageOptions{Limit: 101}}, {PageOptions: client.PageOptions{Cursor: " bad"}}} {
		_, err := c.ReviewOperations(context.Background(), bad)
		checkError(t, err, "validation_failed")
	}
	if calls.Load() != 2 {
		t.Fatal("invalid filter reached service")
	}
}

func TestOperationListClientRejectsMalformedForeignAndBroadenedPages(t *testing.T) {
	base, _ := json.Marshal(client.ReviewOperationsPage{ProjectID: "project-one", Items: []client.ReviewOperationSummary{operationListClientItem()}})
	for name, body := range map[string]string{
		"foreign_project":  strings.Replace(string(base), "project-one", "other-project", 1),
		"unknown_field":    strings.TrimSuffix(string(base), "}") + `,"actor_id":"PRIVATE_ACTOR"}`,
		"missing_field":    strings.Replace(string(base), `"revision":1,`, "", 1),
		"unknown_state":    strings.Replace(string(base), `"state":"queued"`, `"state":"invented"`, 1),
		"terminal_pending": strings.Replace(string(base), `"state":"queued"`, `"state":"completed"`, 1),
		"foreign_channel":  strings.Replace(string(base), `"channel":"cli"`, `"channel":"hosted"`, 1),
		"foreign_pull":     strings.Replace(string(base), `"pull_number":2`, `"pull_number":3`, 1),
		"bad_time":         strings.Replace(string(base), `"updated_at":"2026-10-23T12:00:00Z"`, `"updated_at":"not-a-time"`, 1),
		"missing_cursor":   strings.Replace(string(base), `"has_more":false`, `"has_more":true`, 1),
		"null_items":       `{"project_id":"project-one","items":null,"has_more":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer service.Close()
			c := newClient(t, service)
			_, err := c.ReviewOperations(t.Context(), client.ReviewOperationsOptions{PullNumber: 2, Channel: "cli"})
			checkError(t, err, "invalid_response", "PRIVATE_ACTOR")
		})
	}
}
