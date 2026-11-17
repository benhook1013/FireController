package server_test

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestAssignmentEventsHTTPOptInReplayStreamAndStrictQuery(t *testing.T) {
	f, project, sender, recipient, other := messageSetup(t)
	cursor := snapshotRequest(t, f, project, recipient)
	r := httptest.NewRequest("GET", "/api/v1/projects/"+project+"/message-events", nil)
	r.SetPathValue("project_id", project)
	r.Header.Set("Authorization", "Bearer "+eventClientToken)
	r.Header.Set("X-FC-Worker", sender)
	scope, err := f.store.Scope(r, "write")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.Mutate(t.Context(), scope, core.Mutation{Operation: "test.assignments", Target: "job", Key: "assignment-wire", Request: "synthetic"}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		for _, change := range []struct{ kind, worker string }{{"job.unassigned", recipient}, {"job.assigned", other}, {"job.assigned", recipient}} {
			if err := core.AppendEvent(ctx, tx, scope, change.kind, "job", 2, map[string]string{"recipient_worker_id": change.worker, "sender_worker_id": sender}); err != nil {
				return nil, err
			}
		}
		return struct{}{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/projects/" + project + "/message-events?cursor=" + url.QueryEscape(cursor)
	legacy := request(f.handler, "GET", path, eventClientToken, "", recipient, "")
	explicitFalse := request(f.handler, "GET", path+"&include_assignments=false", eventClientToken, "", recipient, "")
	if legacy.Code != 200 || explicitFalse.Body.String() != legacy.Body.String() {
		t.Fatal("omitted assignment selection changed default message response")
	}
	var page core.EventPage
	selected := request(f.handler, "GET", path+"&include_assignments=true&limit=1", eventClientToken, "", recipient, "")
	if selected.Code != 200 || json.Unmarshal(selected.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].Type != "job.unassigned" || !page.HasMore {
		t.Fatal("finite selected assignment replay lost scope/pagination", selected.Body.String())
	}
	next := request(f.handler, "GET", "/api/v1/projects/"+project+"/message-events?cursor="+url.QueryEscape(page.Cursor)+"&include_assignments=true", eventClientToken, "", recipient, "")
	if next.Code != 200 || json.Unmarshal(next.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].Type != "job.assigned" || page.HasMore {
		t.Fatal("finite assignment continuation failed", next.Body.String())
	}
	s := streamServer(t, f, t.Context())
	for _, selection := range []string{"", "&include_assignments=true"} {
		r, _ := http.NewRequestWithContext(t.Context(), "GET", s.URL+"/api/v1/projects/"+project+"/message-events/stream?cursor="+url.QueryEscape(cursor)+selection, nil)
		r.Header.Set("Authorization", "Bearer "+eventClientToken)
		r.Header.Set("X-FC-Worker", recipient)
		response, err := s.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		page := readPage(t, bufio.NewReader(response.Body))
		response.Body.Close()
		if selection == "" && len(page.Items) != 0 || selection != "" && len(page.Items) != 2 {
			t.Fatal("SSE opt-in disagreed with finite selection", selection, page)
		}
	}
	for _, invalid := range []string{"", "1", "TRUE", "true&include_assignments=false"} {
		query := "cursor=" + url.QueryEscape(cursor) + "&include_assignments=" + invalid
		w := request(f.handler, "GET", "/api/v1/projects/"+project+"/message-events?"+query, eventClientToken, "", recipient, "")
		require(t, w, 400)
		r, _ := http.NewRequestWithContext(t.Context(), "GET", s.URL+"/api/v1/projects/"+project+"/message-events/stream?"+query, nil)
		r.Header.Set("Authorization", "Bearer "+eventClientToken)
		r.Header.Set("X-FC-Worker", recipient)
		response, err := s.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatal("invalid stream assignment selection was accepted", invalid, response.StatusCode)
		}
	}
}
