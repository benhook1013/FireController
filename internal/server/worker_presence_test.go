package server_test

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/server"
)

func workerContactRead(t *testing.T, f *fixture, project string, ids ...string) core.WorkerPresence {
	t.Helper()
	w := request(f.handler, "GET", "/api/v1/projects/"+project+"/workers/presence?"+url.Values{"worker_ids": {strings.Join(ids, ",")}}.Encode(), f.admin, "", "", "")
	require(t, w, 200)
	var v core.WorkerPresence
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func workerContactDetached(t *testing.T, f *fixture, project, worker string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for workerContactRead(t, f, project, worker).Items[0].Attached {
		if time.Now().After(deadline) {
			t.Fatal("contact lease retained after request cancellation")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWorkerPresenceHTTPBoundedSelectorsWireAndReadAuthority(t *testing.T) {
	f := setup(t)
	project := f.project(t, "owner/one")
	other := f.project(t, "owner/two")
	a := f.worker(t, project, "first")
	b := f.worker(t, project, "second")
	foreign := f.worker(t, other, "other")
	token := "synthetic-presence-read-credential-1234"
	credential := f.grant(t, project, token, false)
	path := "/api/v1/projects/" + project + "/workers/presence"
	events, receipts, calls := count(t, f.store, "events"), count(t, f.store, "receipts"), f.calls.Load()
	for _, query := range []string{"", "worker_ids=", "worker_ids=null", "worker_ids=invalid", "worker_ids=" + a + "," + a, "worker_ids=" + a + ",", "worker_ids=" + a + "&worker_ids=" + b, "worker_ids=" + a + "&unknown=x", "worker_ids=%GG", "worker_ids=" + a + "%20", "other=" + a} {
		// Even an absent bearer cannot cause a storage/provider lookup before
		// malformed selectors are rejected.
		require(t, request(f.handler, "GET", path+"?"+query, "", "", "", ""), 400)
	}
	for _, id := range []string{foreign, core.NewID("worker")} {
		require(t, request(f.handler, "GET", path+"?worker_ids="+id, token, "", "", ""), 404)
	}
	require(t, request(f.handler, "GET", path+"?worker_ids="+a, "", "", "", ""), 401)
	w := request(f.handler, "GET", path+"?worker_ids="+b+","+a, token, "", "", "")
	v := require(t, w, 200)
	if len(v) != 5 || v["project_id"] != project || v["complete"] != true || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("presence wire/cache contract")
	}
	items := v["items"].([]any)
	for i, id := range []string{b, a} {
		item := items[i].(map[string]any)
		if len(item) != 3 || item["worker_id"] != id || item["attached"] != false || item["last_seen_at"] != nil {
			t.Fatal("unknown/order wire contract")
		}
	}
	session := strings.Repeat("a", 32)
	cookie := browserLogin(t, f, project, token, session, "localhost")
	require(t, browserRequest(f, "GET", path+"?worker_ids="+a, "", session, "", "localhost", cookie), 200)
	if workerContactRead(t, f, project, a).Items[0].LastSeenAt != nil {
		t.Fatal("browser fabricated agent contact")
	}
	require(t, request(f.handler, "DELETE", "/api/v1/projects/"+project+"/credentials/"+credential, f.admin, core.NewID("key"), "", ""), 200)
	require(t, request(f.handler, "GET", path+"?worker_ids="+a, token, "", "", ""), 401)
	if count(t, f.store, "events") != events+1 || count(t, f.store, "receipts") != receipts+1 || f.calls.Load() != calls {
		t.Fatal("presence read/header/browser work wrote durable state or called provider")
	}
}

func TestWorkerPresenceHTTPMaximumEncodedList(t *testing.T) {
	f := setup(t)
	project := f.project(t, "owner/one")
	ids := make([]string, 100)
	for i := range ids {
		ids[i] = f.worker(t, project, fmt.Sprintf("worker %d", i))
	}
	v := workerContactRead(t, f, project, ids...)
	if len(v.Items) != 100 || v.Items[99].WorkerID != ids[99] {
		t.Fatal("maximum URL-encoded list rejected/reordered")
	}
	ids = append(ids, core.NewID("worker"))
	require(t, request(f.handler, "GET", "/api/v1/projects/"+project+"/workers/presence?"+url.Values{"worker_ids": {strings.Join(ids, ",")}}.Encode(), f.admin, "", "", ""), 400)
}

func TestWorkerPresenceRealRequestLifetimeAndSessionHeader(t *testing.T) {
	f := setup(t)
	project := f.project(t, "owner/one")
	worker := f.worker(t, project, "agent")
	ended := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("project_id", project)
		if _, err := f.store.Scope(r, "read"); err != nil {
			core.WriteError(w, err)
			return
		}
		w.WriteHeader(200)
		http.NewResponseController(w).Flush()
		<-r.Context().Done()
		close(ended)
	})
	web := httptest.NewServer(handler)
	defer web.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", web.URL, nil)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	r.Header.Set("X-FC-Worker", worker)
	r.Header.Set("X-FC-Session", strings.Repeat("b", 32))
	response, err := web.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	v := workerContactRead(t, f, project, worker)
	if response.StatusCode != 200 || !v.Items[0].Attached || v.Items[0].LastSeenAt == nil {
		t.Fatal("real authenticated request was not attached")
	}
	cancel()
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("real request did not join")
	}
	workerContactDetached(t, f, project, worker)
	for _, values := range [][]string{{""}, {strings.Repeat("A", 32)}, {strings.Repeat("c", 32), strings.Repeat("c", 32)}} {
		req := httptest.NewRequest("GET", "/api/v1/projects/"+project+"/workers", nil)
		req.Header.Set("Authorization", "Bearer "+f.admin)
		req.Header.Set("X-FC-Worker", worker)
		req.Header["X-Fc-Session"] = values
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		require(t, rec, 400)
	}
}

func TestWorkerPresenceRealQuietStreamOutlivesAuthenticationDeadline(t *testing.T) {
	f := setup(t)
	project := f.project(t, "owner/one")
	worker := f.worker(t, project, "quiet-agent")
	r := httptest.NewRequest("GET", "/", nil)
	r.SetPathValue("project_id", project)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	r.Header.Set("X-FC-Worker", worker)
	scope, err := f.store.Scope(r, "read")
	if err != nil {
		t.Fatal(err)
	}
	var cursor string
	if err = f.store.Read(context.Background(), scope, func(ctx context.Context, tx *sql.Tx) error {
		cursor, err = f.store.MessageEventCursor(ctx, tx, scope)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	shutdown, cancelShutdown := context.WithCancel(context.Background())
	defer cancelShutdown()
	mux := http.NewServeMux()
	server.RegisterMessageStream(mux, f.store, shutdown)
	mux.Handle("/", f.handler)
	web := httptest.NewServer(mux)
	defer web.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", web.URL+"/api/v1/projects/"+project+"/message-events/stream?cursor="+url.QueryEscape(cursor), nil)
	req.Header.Set("Authorization", "Bearer "+f.admin)
	req.Header.Set("X-FC-Worker", worker)
	req.Header.Set("X-FC-Session", strings.Repeat("d", 32))
	response, err := web.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("stream status %d", response.StatusCode)
	}
	if _, err = nextStreamFrame(bufio.NewReader(response.Body)); err != nil {
		t.Fatal(err)
	}
	events, receipts, calls := count(t, f.store, "events"), count(t, f.store, "receipts"), f.calls.Load()
	// Preserve the production two-second authentication timeout. Quiet wait has
	// no model-visible heartbeat and remains attached beyond that deadline.
	select {
	case <-time.After(2100 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal("quiet stream ended early")
	}
	v := workerContactRead(t, f, project, worker)
	if !v.Items[0].Attached {
		t.Fatal("short authentication context released quiet stream contact")
	}
	cancel()
	response.Body.Close()
	workerContactDetached(t, f, project, worker)
	if count(t, f.store, "events") != events || count(t, f.store, "receipts") != receipts || f.calls.Load() != calls {
		t.Fatal("quiet contact changed durable state or contacted provider")
	}
}
