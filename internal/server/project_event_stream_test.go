package server_test

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/server"
)

func projectStreamScope(t *testing.T, f *fixture, project, worker, token string) core.Scope {
	t.Helper()
	r := httptest.NewRequest("GET", "http://synthetic", nil)
	r.SetPathValue("project_id", project)
	r.Header.Set("Authorization", "Bearer "+token)
	if worker != "" {
		r.Header.Set("X-FC-Worker", worker)
	}
	scope, err := f.store.Scope(r, "read")
	if err != nil {
		t.Fatal(err)
	}
	return scope
}
func projectStreamCursor(t *testing.T, f *fixture, project, worker, token string) string {
	t.Helper()
	scope := projectStreamScope(t, f, project, worker, token)
	var cursor string
	err := f.store.Read(context.Background(), scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		cursor, err = f.store.ProjectEventCursor(ctx, tx, scope)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return cursor
}
func appendProjectStreamEvent(t *testing.T, f *fixture, project, kind, id string) {
	t.Helper()
	scope := projectStreamScope(t, f, project, "", eventClientToken)
	_, err := f.store.Mutate(context.Background(), scope, core.Mutation{Operation: "test.metadata", Target: id, Key: id, Request: kind}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		return map[string]string{"id": id}, core.AppendEvent(ctx, tx, scope, kind, id, 3, map[string]string{"private": "not stream content"})
	})
	if err != nil {
		t.Fatal(err)
	}
}
func projectStreamServer(t *testing.T, f *fixture, shutdown context.Context) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	server.RegisterMessageStream(mux, f.store, shutdown)
	server.RegisterProjectStream(mux, f.store, shutdown)
	s := httptest.NewUnstartedServer(mux)
	s.Config.ReadTimeout = 50 * time.Millisecond
	s.Config.WriteTimeout = 50 * time.Millisecond
	s.Start()
	t.Cleanup(s.Close)
	return s
}
func openProjectStream(t *testing.T, s *httptest.Server, project, worker, token, query string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	r, _ := http.NewRequestWithContext(ctx, "GET", s.URL+"/api/v1/projects/"+project+"/project-events/stream?"+query, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if worker != "" {
		r.Header.Set("X-FC-Worker", worker)
	}
	response, err := s.Client().Do(r)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); response.Body.Close() })
	return response, cancel
}
func readProjectStreamPage(t *testing.T, r *bufio.Reader) core.ProjectEventPage {
	t.Helper()
	frame, err := nextStreamFrame(r)
	if err != nil {
		t.Fatal(err)
	}
	var page core.ProjectEventPage
	if frame.event != "project-events" || frame.id == "" || json.Unmarshal([]byte(frame.data), &page) != nil || page.EventCursor != frame.id || page.Items == nil {
		t.Fatalf("invalid metadata frame: %+v", frame)
	}
	var raw map[string]json.RawMessage
	if err = json.Unmarshal([]byte(frame.data), &raw); err != nil || len(raw) != 3 {
		t.Fatal("metadata envelope changed")
	}
	for _, item := range page.Items {
		if item.EventCursor == "" || item.ResourceID == "" || item.Revision != 3 {
			t.Fatalf("invalid metadata item: %+v", item)
		}
	}
	if strings.Contains(frame.data, "private") || strings.Contains(frame.data, "payload") || strings.Contains(frame.data, "actor_id") {
		t.Fatal("metadata leaked private content")
	}
	return page
}

func TestProjectStreamHTTPDefaultPagingGapReplayAndWorkerIndependence(t *testing.T) {
	f, project, sender, recipient, other := messageSetup(t)
	cursor := projectStreamCursor(t, f, project, sender, eventClientToken)
	// Snapshot-to-registration gap is recovered through durable replay, not hints.
	for i := 0; i < 26; i++ {
		appendProjectStreamEvent(t, f, project, "map.entry_created", fmt.Sprintf("entry-%02d", i))
	}
	sendMessage(t, f, project, sender, recipient, "private irrelevant message", "metadata-gap")
	receipts, events := count(t, f.store, "receipts"), count(t, f.store, "events")
	s := projectStreamServer(t, f, context.Background())
	response, cancel := openProjectStream(t, s, project, other, eventClientToken, "cursor="+url.QueryEscape(cursor))
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("metadata stream headers")
	}
	reader := bufio.NewReader(response.Body)
	first := readProjectStreamPage(t, reader)
	last := readProjectStreamPage(t, reader)
	if len(first.Items) != 25 || !first.HasMore || len(last.Items) != 1 || last.HasMore || first.Items[0].ResourceID != "entry-00" || last.Items[0].ResourceID != "entry-25" {
		t.Fatalf("default bounded replay: %+v %+v", first, last)
	}
	if first.EventCursor != first.Items[24].EventCursor || last.EventCursor == last.Items[0].EventCursor {
		t.Fatal("wrong continuation or filtered-gap watermark")
	}
	cancel()
	response.Body.Close()
	// Per-item cursor resumes midway, without a selected worker or implicit ack.
	response, cancel = openProjectStream(t, s, project, "", eventClientToken, "cursor="+url.QueryEscape(first.Items[23].EventCursor)+"&limit=1")
	reader = bufio.NewReader(response.Body)
	one := readProjectStreamPage(t, reader)
	two := readProjectStreamPage(t, reader)
	if len(one.Items) != 1 || one.Items[0].ResourceID != "entry-24" || len(two.Items) != 1 || two.Items[0].ResourceID != "entry-25" {
		t.Fatal("per-item replay lost events")
	}
	// Ordinary server idle deadlines must not terminate the persistent stream.
	time.Sleep(150 * time.Millisecond)
	appendProjectStreamEvent(t, f, project, "project.presentation_changed", "presentation-after-idle")
	update := readProjectStreamPage(t, reader)
	if len(update.Items) != 1 || update.Items[0].ResourceID != "presentation-after-idle" {
		t.Fatal("idle stream lost commit")
	}
	cancel()
	response.Body.Close()
	if count(t, f.store, "receipts") != receipts+1 || count(t, f.store, "events") != events+1 || f.calls.Load() != 1 {
		t.Fatal("stream performed writes or provider work")
	}
}

func TestProjectStreamHTTPValidationNamespacesAndScope(t *testing.T) {
	f, project, _, recipient, _ := messageSetup(t)
	cursor := projectStreamCursor(t, f, project, "", eventClientToken)
	messageCursor := snapshotRequest(t, f, project, recipient)
	foreign := f.project(t, "owner/two")
	foreignToken := "synthetic-foreign-metadata-principal-token"
	f.grant(t, foreign, foreignToken, true)
	foreignCursor := projectStreamCursor(t, f, foreign, "", foreignToken)
	otherToken := "synthetic-other-metadata-principal-token"
	f.grant(t, project, otherToken, false)
	principalCursor := projectStreamCursor(t, f, project, "", otherToken)
	s := projectStreamServer(t, f, context.Background())
	beforeReceipts, beforeEvents := count(t, f.store, "receipts"), count(t, f.store, "events")
	for _, query := range []string{"", "cursor=", "cursor=bad", "cursor=" + url.QueryEscape(cursor) + "&unknown=x", "cursor=" + url.QueryEscape(cursor) + "&cursor=" + url.QueryEscape(cursor), "cursor=" + url.QueryEscape(cursor) + "&limit=0", "cursor=" + url.QueryEscape(cursor) + "&limit=1&limit=2", "cursor=" + url.QueryEscape(messageCursor), "cursor=" + url.QueryEscape(foreignCursor), "cursor=" + url.QueryEscape(principalCursor)} {
		response, cancel := openProjectStream(t, s, project, "", eventClientToken, query)
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		cancel()
		if err != nil || response.StatusCode != 400 || response.Header.Get("Content-Type") != "application/json" || !strings.Contains(string(data), "validation_failed") || strings.Contains(string(data), eventClientToken) {
			t.Fatalf("invalid metadata request: %d %s %v", response.StatusCode, data, err)
		}
	}
	// A recorder lacks bounded transport support; refusal is before SSE headers.
	require(t, request(s.Config.Handler, "GET", "/api/v1/projects/"+project+"/project-events/stream?cursor="+url.QueryEscape(cursor), eventClientToken, "", "", ""), 503)
	require(t, request(s.Config.Handler, "HEAD", "/api/v1/projects/"+project+"/project-events/stream?cursor="+url.QueryEscape(cursor), eventClientToken, "", "", ""), 405)
	require(t, request(s.Config.Handler, "GET", "/api/v1/projects/"+project+"/project-events/stream?cursor="+url.QueryEscape(cursor), "", "", "", ""), 401)
	if beforeReceipts != count(t, f.store, "receipts") || beforeEvents != count(t, f.store, "events") {
		t.Fatal("refused reads wrote state")
	}
}

func TestProjectStreamHTTPRevocationAndShutdown(t *testing.T) {
	for _, revoke := range []bool{true, false} {
		t.Run(fmt.Sprint(revoke), func(t *testing.T) {
			f, project, _, _, _ := messageSetup(t)
			token := "synthetic-revocable-project-stream-token"
			id := f.grant(t, project, token, false)
			cursor := projectStreamCursor(t, f, project, "", token)
			shutdown, stop := context.WithCancel(context.Background())
			defer stop()
			s := projectStreamServer(t, f, shutdown)
			response, cancel := openProjectStream(t, s, project, "", token, "cursor="+url.QueryEscape(cursor))
			defer cancel()
			reader := bufio.NewReader(response.Body)
			ready := readProjectStreamPage(t, reader)
			if len(ready.Items) != 0 || ready.EventCursor != cursor {
				t.Fatal("initial readiness missing")
			}
			if revoke {
				require(t, request(f.handler, "DELETE", "/api/v1/projects/"+project+"/credentials/"+id, f.admin, "metadata-revoke", "", ""), 200)
				frame, err := nextStreamFrame(reader)
				if err != nil || frame.event != "error" || frame.id != "" || !strings.Contains(frame.data, "unauthorized") || strings.Contains(frame.data, token) || strings.Contains(frame.data, f.dir) {
					t.Fatalf("unsafe revoke frame: %+v %v", frame, err)
				}
			} else {
				stop()
			}
			if _, err := reader.ReadByte(); err != io.EOF {
				t.Fatalf("owned stream not joined: %v", err)
			}
		})
	}
}

// This transport deliberately blocks Flush until its installed deadline. It
// exercises the project branch of the shared write/flush bound; the existing
// incoming-message native-socket test separately covers a nonreading TCP peer.
type slowProjectFlush struct {
	*httptest.ResponseRecorder
	deadline        time.Time
	writes, flushes int
}

func (w *slowProjectFlush) SetWriteDeadline(at time.Time) error { w.deadline = at; return nil }
func (w *slowProjectFlush) Write(data []byte) (int, error) {
	w.writes++
	return w.ResponseRecorder.Write(data)
}
func (w *slowProjectFlush) FlushError() error {
	w.flushes++
	if w.deadline.IsZero() {
		return errors.New("flush has no deadline")
	}
	timer := time.NewTimer(time.Until(w.deadline))
	defer timer.Stop()
	<-timer.C
	return os.ErrDeadlineExceeded
}
func TestProjectStreamSlowFlushUsesSharedDeadline(t *testing.T) {
	f, project, _, _, _ := messageSetup(t)
	cursor := projectStreamCursor(t, f, project, "", eventClientToken)
	mux := http.NewServeMux()
	server.RegisterProjectStream(mux, f.store, context.Background())
	r := httptest.NewRequest("GET", "/api/v1/projects/"+project+"/project-events/stream?cursor="+url.QueryEscape(cursor), nil)
	r.Header.Set("Authorization", "Bearer "+eventClientToken)
	w := &slowProjectFlush{ResponseRecorder: httptest.NewRecorder()}
	start := time.Now()
	mux.ServeHTTP(w, r)
	elapsed := time.Since(start)
	if elapsed < 1500*time.Millisecond || elapsed > 4*time.Second || w.Code != 200 || w.flushes != 1 || w.writes != 1 || !w.deadline.IsZero() {
		t.Fatalf("unbounded/duplicate transport: %v %+v", elapsed, w)
	}
}

func TestProjectStreamHTTPBrowserSessionReadAuthority(t *testing.T) {
	f, project, _, _, _ := messageSetup(t)
	cursor := projectStreamCursor(t, f, project, "", eventClientToken)
	s := projectStreamServer(t, f, context.Background())
	base, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := strings.Repeat("a", 32)
	cookie := browserLoginCapabilities(t, f, project, eventClientToken, sessionID, base.Host, true, false)
	receipts, events := count(t, f.store, "receipts"), count(t, f.store, "events")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", s.URL+"/api/v1/projects/"+project+"/project-events/stream?cursor="+url.QueryEscape(cursor), nil)
	r.Header.Set("X-FC-Browser-Session", sessionID)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	r.AddCookie(cookie)
	response, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("browser stream status %d", response.StatusCode)
	}
	page := readProjectStreamPage(t, bufio.NewReader(response.Body))
	if len(page.Items) != 0 || page.EventCursor != cursor {
		t.Fatal("browser snapshot binding changed")
	}
	cancel()
	response.Body.Close()
	if receipts != count(t, f.store, "receipts") || events != count(t, f.store, "events") || f.calls.Load() != 1 {
		t.Fatal("browser stream mutated domain or provider")
	}
}
