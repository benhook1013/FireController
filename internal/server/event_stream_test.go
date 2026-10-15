package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/server"
)

type streamFrame struct{ event, id, data string }

func nextStreamFrame(reader *bufio.Reader) (streamFrame, error) {
	var frame streamFrame
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return frame, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return frame, nil
		}
		if strings.HasPrefix(line, "event: ") {
			frame.event = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "id: ") {
			frame.id = strings.TrimPrefix(line, "id: ")
		} else if strings.HasPrefix(line, "data: ") {
			frame.data = strings.TrimPrefix(line, "data: ")
		}
	}
}
func streamServer(t *testing.T, f *fixture, shutdownContext context.Context) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	server.RegisterMessageStream(mux, f.store, shutdownContext)
	s := httptest.NewUnstartedServer(mux)
	s.Config.ReadTimeout = 50 * time.Millisecond
	s.Config.WriteTimeout = 50 * time.Millisecond
	s.Start()
	t.Cleanup(s.Close)
	return s
}
func openStream(t *testing.T, s *httptest.Server, project, worker, cursor string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	r, _ := http.NewRequestWithContext(ctx, "GET", s.URL+"/api/v1/projects/"+project+"/message-events/stream?cursor="+url.QueryEscape(cursor)+"&limit=1", nil)
	r.Header.Set("Authorization", "Bearer "+eventClientToken)
	r.Header.Set("X-FC-Worker", worker)
	response, err := s.Client().Do(r)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); response.Body.Close() })
	return response, cancel
}
func readPage(t *testing.T, reader *bufio.Reader) core.EventPage {
	t.Helper()
	frame, err := nextStreamFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	var page core.EventPage
	if frame.event != "message-events" || frame.id == "" || json.Unmarshal([]byte(frame.data), &page) != nil || page.Cursor != frame.id {
		t.Fatalf("bad SSE frame: %+v", frame)
	}
	return page
}

func TestMessageStreamActualHTTPReadyCommitReplayAndIdleTimeouts(t *testing.T) {
	f, project, sender, recipient, _ := messageSetup(t)
	cursor := snapshotRequest(t, f, project, recipient)
	s := streamServer(t, f, context.Background())
	response, cancel := openStream(t, s, project, recipient, cursor)
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatal("stream headers")
	}
	reader := bufio.NewReader(response.Body)
	initial := readPage(t, reader)
	if len(initial.Items) != 0 || initial.Cursor != cursor {
		t.Fatal("initial ready page")
	}
	// Exceed both ordinary request-read and whole-response deadlines. A frame
	// must still flush after this idle period; only in-flight writes are bounded.
	time.Sleep(3 * max(s.Config.ReadTimeout, s.Config.WriteTimeout))
	message := sendMessage(t, f, project, sender, recipient, "private stream body", "stream-message-one")
	page := readPage(t, reader)
	if len(page.Items) != 1 || page.Items[0].ResourceID != message.ID || strings.Contains(string(page.Items[0].Payload), "private stream body") {
		t.Fatal("committed event/body scope")
	}
	cancel()
	response.Body.Close()
	replay, replayCancel := openStream(t, s, project, recipient, cursor)
	defer replayCancel()
	again := readPage(t, bufio.NewReader(replay.Body))
	if len(again.Items) != 1 || again.Items[0].Sequence != page.Items[0].Sequence {
		t.Fatal("processed-cursor replay lost event")
	}
	if f.calls.Load() != 1 {
		t.Fatal("stream performed provider work")
	}
}

func TestMessageStreamStopsOnServiceShutdownWithoutAcknowledgingAndReplaysAfterReconnect(t *testing.T) {
	f, project, sender, recipient, _ := messageSetup(t)
	cursor := snapshotRequest(t, f, project, recipient)
	shutdownContext, stopService := context.WithCancel(context.Background())
	s := streamServer(t, f, shutdownContext)
	response, _ := openStream(t, s, project, recipient, cursor)
	reader := bufio.NewReader(response.Body)
	initial := readPage(t, reader)
	if len(initial.Items) != 0 || initial.Cursor != cursor {
		t.Fatal("initial stream page")
	}

	stopService()
	closed := make(chan error, 1)
	go func() {
		_, err := reader.ReadByte()
		closed <- err
	}()
	select {
	case err := <-closed:
		if err != io.EOF {
			t.Fatalf("shutdown stream read: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("service shutdown left the persistent stream open")
	}
	_ = response.Body.Close()

	receipt := sendMessage(t, f, project, sender, recipient, "reconnect-safe body", "shutdown-stream-message")
	assertUnreadUnacknowledged := func() {
		t.Helper()
		page := require(t, request(f.handler, http.MethodGet, "/api/v1/projects/"+project+"/inbox/"+recipient, eventClientToken, "", recipient, ""), http.StatusOK)
		items, ok := page["items"].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("inbox after stream close: %#v", page)
		}
		item := items[0].(map[string]any)
		if item["id"] != receipt.ID || item["seen"] != false || item["acknowledged"] != false {
			t.Fatalf("stream changed recipient state: %#v", item)
		}
	}
	assertUnreadUnacknowledged()

	reconnectServer := streamServer(t, f, context.Background())
	replay, _ := openStream(t, reconnectServer, project, recipient, cursor)
	page := readPage(t, bufio.NewReader(replay.Body))
	if len(page.Items) != 1 || page.Items[0].ResourceID != receipt.ID || strings.Contains(string(page.Items[0].Payload), "reconnect-safe body") {
		t.Fatalf("reconnect did not replay the committed compact event: %+v", page)
	}
	assertUnreadUnacknowledged()
	_ = replay.Body.Close()
}

func TestMessageStreamValidationAndUnsupportedTransportStayJSON(t *testing.T) {
	f, project, _, recipient, other := messageSetup(t)
	cursor := snapshotRequest(t, f, project, recipient)
	s := streamServer(t, f, context.Background())
	for _, query := range []string{"", "cursor=", "cursor=" + url.QueryEscape(cursor) + "&unknown=1", "cursor=" + url.QueryEscape(cursor) + "&cursor=" + url.QueryEscape(cursor), "cursor=" + url.QueryEscape(cursor) + "&limit=1&limit=2", "cursor=" + url.QueryEscape(cursor) + "&wait_ms=1", "cursor=" + url.QueryEscape(cursor) + "&limit=0", "cursor=invalid"} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		r, _ := http.NewRequestWithContext(ctx, "GET", s.URL+"/api/v1/projects/"+project+"/message-events/stream?"+query, nil)
		r.Header.Set("Authorization", "Bearer "+eventClientToken)
		r.Header.Set("X-FC-Worker", recipient)
		response, err := s.Client().Do(r)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		cancel()
		if response.StatusCode != 400 || response.Header.Get("Content-Type") != "application/json" || !json.Valid(data) || strings.Contains(string(data), eventClientToken) {
			t.Fatalf("invalid stream request: %d %s", response.StatusCode, data)
		}
	}
	// Selected-worker signed cursors cannot route another worker's stream.
	r := httptest.NewRequest("GET", "/api/v1/projects/"+project+"/message-events/stream?cursor="+url.QueryEscape(cursor), nil)
	r.Header.Set("Authorization", "Bearer "+eventClientToken)
	r.Header.Set("X-FC-Worker", other)
	w := httptest.NewRecorder()
	s.Config.Handler.ServeHTTP(w, r)
	// Recorder cannot expose deadlines; test this valid transport capability
	// refusal separately from actual HTTP cursor validation above.
	if w.Code != 503 || !strings.Contains(w.Body.String(), "stream_unavailable") {
		t.Fatal("unsupported controller did not fail safely")
	}
}

func TestMessageStreamRevocationSendsOneSafeErrorThenCloses(t *testing.T) {
	f, project, _, recipient, _ := messageSetup(t)
	token := "synthetic-revocable-stream-token-12345"
	id := f.grant(t, project, token, false)
	cursor := snapshotCursorFor(t, f, project, recipient, token)
	s := streamServer(t, f, context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", s.URL+"/api/v1/projects/"+project+"/message-events/stream?cursor="+url.QueryEscape(cursor), nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-FC-Worker", recipient)
	response, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	readPage(t, reader)
	require(t, request(f.handler, "DELETE", "/api/v1/projects/"+project+"/credentials/"+id, f.admin, "stream-revoke", "", ""), 200)
	frame, err := nextStreamFrame(reader)
	if err != nil || frame.event != "error" || frame.id != "" {
		t.Fatalf("missing safe stream error: %+v %v", frame, err)
	}
	var failure struct {
		Error     *core.Error `json:"error"`
		RequestID string      `json:"request_id"`
	}
	if json.Unmarshal([]byte(frame.data), &failure) != nil || failure.Error.Code != "unauthorized" || failure.RequestID == "" || strings.Contains(frame.data, token) || strings.Contains(frame.data, f.dir) {
		t.Fatal("unsafe revocation diagnostic")
	}
	if _, err = reader.ReadByte(); err != io.EOF {
		t.Fatalf("stream not closed after one error: %v", err)
	}
}

func TestMessageStreamBoundsFrameBeforeInitialHeaders(t *testing.T) {
	f, project, _, recipient, _ := messageSetup(t)
	cursor := snapshotRequest(t, f, project, recipient)
	payload := `{"recipient_worker_id":"` + recipient + `","synthetic":"` + strings.Repeat("x", 1<<20) + `"}`
	_, err := f.store.DB().Exec("INSERT INTO events(project_id,event_type,resource_id,revision,actor_id,payload_json,created_at) VALUES(?,?,?,?,?,?,?)", project, "message.sent", "synthetic-large", 1, "synthetic", payload, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	s := streamServer(t, f, context.Background())
	response, _ := openStream(t, s, project, recipient, cursor)
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != 503 || response.Header.Get("Content-Type") != "application/json" || !strings.Contains(string(data), "stream_frame_too_large") {
		t.Fatalf("oversize frame not rejected before headers: %d", response.StatusCode)
	}
}

func TestMessageStreamSlowNativeSocketWriteHasDeadline(t *testing.T) {
	f, project, _, recipient, _ := messageSetup(t)
	cursor := snapshotRequest(t, f, project, recipient)
	payload := `{"recipient_worker_id":"` + recipient + `","synthetic":"` + strings.Repeat("x", 8<<10) + `"}`
	_, err := f.store.DB().Exec(`WITH RECURSIVE numbers(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM numbers WHERE x<3000) INSERT INTO events(project_id,event_type,resource_id,revision,actor_id,payload_json,created_at) SELECT ?,'message.sent','slow_'||x,1,'synthetic',?,? FROM numbers`, project, payload, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	server.RegisterMessageStream(mux, f.store, context.Background())
	var status atomic.Int32
	var attempted atomic.Int64
	done := make(chan struct{}, 1)
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { done <- struct{}{} }()
		mux.ServeHTTP(&observedStreamWriter{ResponseWriter: w, status: &status, attempted: &attempted}, r)
	}))
	s.Config.WriteTimeout = 10 * time.Second
	s.Start()
	t.Cleanup(s.Close)
	conn, err := net.DialTimeout("tcp", s.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tcp := conn.(*net.TCPConn)
	tcp.SetReadBuffer(1024)
	conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, err = fmt.Fprintf(conn, "GET /api/v1/projects/%s/message-events/stream?cursor=%s&limit=100 HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nX-FC-Worker: %s\r\n\r\n", project, url.QueryEscape(cursor), s.Listener.Addr(), eventClientToken, recipient)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	select {
	case <-done:
		if time.Since(start) < 1500*time.Millisecond || time.Since(start) > 4*time.Second || status.Load() != 200 || attempted.Load() < 1<<19 {
			t.Fatal("write deadline exceeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("non-reading socket retained stream beyond write bound")
	}
}

type observedStreamWriter struct {
	http.ResponseWriter
	status    *atomic.Int32
	attempted *atomic.Int64
}

func (w *observedStreamWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observedStreamWriter) WriteHeader(status int) {
	w.status.Store(int32(status))
	w.ResponseWriter.WriteHeader(status)
}
func (w *observedStreamWriter) Write(data []byte) (int, error) {
	w.attempted.Add(int64(len(data)))
	return w.ResponseWriter.Write(data)
}

func TestMessageStreamHEADReturnsPromptlyWithoutSubscription(t *testing.T) {
	f, project, _, recipient, _ := messageSetup(t)
	s := streamServer(t, f, context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodHead, s.URL+"/api/v1/projects/"+project+"/message-events/stream", nil)
	r.Header.Set("Authorization", "Bearer "+eventClientToken)
	r.Header.Set("X-FC-Worker", recipient)
	response, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 405 || response.Header.Get("Allow") != "GET" || response.Header.Get("Content-Type") != "application/json" {
		t.Fatal("HEAD opened an event subscription")
	}
}
