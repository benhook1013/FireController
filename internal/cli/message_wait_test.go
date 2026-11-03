package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/server"
)

type quietOutput struct {
	mu sync.Mutex
	bytes.Buffer
}

func (o *quietOutput) Write(b []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.Buffer.Write(b)
}
func (o *quietOutput) text() string { o.mu.Lock(); defer o.mu.Unlock(); return o.Buffer.String() }

func TestMessageWaitCommandQuietPublicDeliveryTimeoutAndCancellation(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "quiet-wait")
	sender := f.worker(t, project, "Sender")
	recipient := f.worker(t, project, "Recipient")
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	streamCtx, stopStreams := context.WithCancel(t.Context())
	server.RegisterMessageStream(mux, f.store, streamCtx)
	entered, released := make(chan struct{}, 4), make(chan struct{}, 4)
	var streams, writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/stream") {
			streams.Add(1)
			entered <- struct{}{}
			defer func() { released <- struct{}{} }()
		}
		mux.ServeHTTP(w, r)
	}))
	defer service.Close()
	defer stopStreams()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"recipient": {project, recipient}})
	snapshot := f.call(t, "GET", "/api/v1/projects/"+project+"/inbox/"+recipient+"/snapshot", f.admin, recipient, nil)
	cursor := snapshot["event_cursor"].(string)
	start := func(ctx context.Context, extra ...string) (*quietOutput, *quietOutput, <-chan int) {
		t.Helper()
		out, diag := &quietOutput{}, &quietOutput{}
		done := make(chan int, 1)
		args := append([]string{"--profiles-file", path, "--profile", "recipient", "inbox", "wait", "--cursor", cursor}, extra...)
		go func() { done <- cli.RunContext(ctx, args, nil, out, diag) }()
		return out, diag, done
	}
	out, diag, done := start(t.Context()) // Default wait300; delivery ends the stream early.
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("stream not opened")
	}
	select {
	case code := <-done:
		t.Fatal("empty readiness returned", code, diag.text())
	case <-time.After(100 * time.Millisecond):
	}
	if out.text() != "" || diag.text() != "" {
		t.Fatal("intermediate output")
	}
	id, _ := f.sendMessage(t, project, sender, recipient, "Private body never emitted")
	select {
	case code := <-done:
		if code != cli.ExitSuccess {
			t.Fatal(code, diag.text())
		}
	case <-time.After(time.Second):
		t.Fatal("delivery did not finish")
	}
	var page client.MessageEventPage
	if json.Unmarshal([]byte(out.text()), &page) != nil || len(page.Items) != 1 || page.Items[0].ResourceID != id || page.Items[0].Type != "message.sent" || page.TimedOut || strings.Count(out.text(), "\n") != 1 || strings.Contains(out.text(), "Private body") || diag.text() != "" {
		t.Fatal("quiet final result", out.text(), diag.text())
	}
	<-released
	cursor = page.Cursor
	// Expiry is a successful one-page result, after stream readiness, without effects.
	var before int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM events").Scan(&before); err != nil {
		t.Fatal(err)
	}
	out, diag, done = start(t.Context(), "--wait-seconds", "1", "--limit", "1")
	select {
	case code := <-done:
		if code != cli.ExitSuccess {
			t.Fatal(code, diag.text())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout did not settle")
	}
	if json.Unmarshal([]byte(out.text()), &page) != nil || !page.TimedOut || len(page.Items) != 0 || page.Cursor == "" || page.HasMore || diag.text() != "" {
		t.Fatal("timeout result", out.text(), diag.text())
	}
	<-released
	<-entered
	cursor = page.Cursor
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out, diag, done = start(ctx, "--wait-seconds", "900", "--limit", "100")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("cancellable stream not opened")
	}
	cancel()
	select {
	case code := <-done:
		if code != cli.ExitCancelled || out.text() != "" || !strings.Contains(diag.text(), `"code":"canceled"`) || strings.Contains(diag.text(), "outcome_unknown") {
			t.Fatal(code, diag.text())
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not return")
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("stream leaked")
	}
	var after, seen, ack int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM events").Scan(&after); err != nil || after != before {
		t.Fatal("wait wrote events", err, before, after)
	}
	if err := f.store.DB().QueryRow("SELECT seen,acknowledged FROM inbox_recipient_state WHERE message_id=?", id).Scan(&seen, &ack); err != nil || seen != 0 || ack != 0 {
		t.Fatal("wait acknowledged", err)
	}
	if streams.Load() != 3 || writes.Load() != 0 || f.resolverHit.Load() != 1 {
		t.Fatal("retry/provider/write", streams.Load(), writes.Load())
	}
}

func TestMessageWaitCommandRejectsBeforeProfileOrHTTP(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	f := setupServiceFixture(t)
	p := f.project(t, "invalid-wait")
	w := f.worker(t, p, "Bound")
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"bound": {p, w}})
	cases := [][]string{nil, {"--cursor"}, {"--cursor", ""}, {"--cursor", " x"}, {"--cursor", "x\n"}, {"--cursor", string([]byte{255})}, {"--cursor", strings.Repeat("é", 2048) + "x"}, {"--cursor", "x", "--cursor", "y"}, {"--cursor", "x", "--limit", "0"}, {"--cursor", "x", "--limit", "101"}, {"--cursor", "x", "--wait-seconds", "0"}, {"--cursor", "x", "--wait-seconds", "901"}, {"--cursor", "x", "--wait-seconds", "null"}, {"--cursor", "x", "--wait-seconds", "1.0"}, {"--cursor", "x", "--wait-seconds", "1", "--wait-seconds", "2"}, {"--cursor", "x", "--worker", "other"}, {"--cursor", "x", "--unread"}, {"--cursor", "x", "extra"}}
	for _, args := range cases {
		for _, profiles := range []string{path, "/unavailable/profile"} {
			full := append([]string{"--profiles-file", profiles, "--profile", "bound", "inbox", "wait"}, args...)
			code, out, err := run(full, nil)
			if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_arguments") {
				t.Fatal(args, code, out, err)
			}
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid arguments used HTTP")
	}
}

func TestMessageWaitCommandExtendsOnlyExplicitWaitBudget(t *testing.T) {
	// A genuine quiet expiry crosses the normal five-second command budget.
	f := setupServiceFixture(t)
	p := f.project(t, "long-quiet-wait")
	w := f.worker(t, p, "Bound")
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	streamCtx, stopStreams := context.WithCancel(t.Context())
	server.RegisterMessageStream(mux, f.store, streamCtx)
	service := httptest.NewServer(mux)
	defer service.Close()
	defer stopStreams()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"bound": {p, w}})
	snap := f.call(t, "GET", "/api/v1/projects/"+p+"/inbox/"+w+"/snapshot", f.admin, w, nil)
	started := time.Now()
	code, out, diag := mutationInvoke(t, path, "bound", "inbox", "wait", "--cursor", snap["event_cursor"].(string), "--wait-seconds", "6")
	var page client.MessageEventPage
	if code != cli.ExitSuccess || diag != "" || json.Unmarshal([]byte(out), &page) != nil || !page.TimedOut || time.Since(started) < 6*time.Second || time.Since(started) > 8*time.Second {
		t.Fatal("extended wait", code, diag, time.Since(started))
	}
}

func TestMessageWaitCommandStreamResyncIsConflictWithoutEffects(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "resync-wait")
	sender := f.worker(t, project, "Sender")
	recipient := f.worker(t, project, "Recipient")
	snapshotPath := "/api/v1/projects/" + project + "/inbox/" + recipient + "/snapshot"
	original := f.call(t, "GET", snapshotPath, f.admin, recipient, nil)["event_cursor"].(string)
	message, _ := f.sendMessage(t, project, sender, recipient, "PRIVATE_PENDING_MESSAGE")
	advanced := f.call(t, "GET", snapshotPath, f.admin, recipient, nil)["event_cursor"].(string)
	if original == advanced {
		t.Fatal("fixture did not advance its event cursor")
	}
	var streams, writes atomic.Int64
	released := make(chan struct{}, 2)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/stream") {
			streams.Add(1)
			defer func() { released <- struct{}{} }()
			if r.URL.Path != "/api/v1/projects/"+project+"/message-events/stream" || r.URL.Query().Get("cursor") != original || r.URL.Query().Get("limit") != "20" || len(r.URL.Query()) != 3 || r.URL.Query().Get("include_assignments") != "true" || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("X-FC-Worker") != recipient || r.Header.Get("Authorization") != "Bearer "+f.admin {
				t.Error("stream changed its original cursor or scoped binding")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			page, _ := json.Marshal(client.MessageEventPage{Items: []client.MessageEvent{}, Cursor: advanced})
			_, _ = fmt.Fprintf(w, "event: message-events\nid: %s\ndata: %s\n\nevent: error\ndata: {\"error\":{\"code\":\"resync_required\",\"message\":\"UNTRUSTED_RESYNC_TEXT\"},\"request_id\":\"request-resync\"}\n\n", advanced, page)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"recipient": {project, recipient}})
	profilesBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Capture actual retained message/handling bytes and all receipt/event counts.
	retained := func() string {
		t.Helper()
		var value string
		err := f.store.DB().QueryRow(`SELECT json_array(m.body,m.created_at,s.seen,s.acknowledged,s.revision,
		 (SELECT count(*) FROM inbox_messages),(SELECT count(*) FROM inbox_recipient_state),
		 (SELECT count(*) FROM receipts),(SELECT count(*) FROM events))
		 FROM inbox_messages m JOIN inbox_recipient_state s ON s.project_id=m.project_id AND s.message_id=m.id
		 WHERE m.project_id=? AND m.id=?`, project, message).Scan(&value)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := retained()
	for range 2 { // Explicit replay still sends the caller's original cursor.
		code, out, diag := mutationInvoke(t, path, "recipient", "inbox", "wait", "--cursor", original)
		var result struct {
			Error client.Error `json:"error"`
		}
		if code != cli.ExitConflict || out != "" || json.Unmarshal([]byte(diag), &result) != nil || result.Error.Code != "resync_required" || result.Error.RequestID != "request-resync" || result.Error.Action != "Read a fresh bound-worker inbox snapshot, then continue from its event cursor; pending messages remain unacknowledged." || result.Error.OutcomeUnknown || strings.Count(diag, "\n") != 1 {
			t.Fatalf("stream resync must be one safe conflict diagnostic: exit=%d stdout=%q stderr=%q", code, out, diag)
		}
		for _, private := range []string{original, advanced, "UNTRUSTED_RESYNC_TEXT", "PRIVATE_PENDING_MESSAGE", f.admin} {
			if strings.Contains(diag, private) {
				t.Fatal("resync diagnostic exposed cursor, body or credential")
			}
		}
		select {
		case <-released:
		case <-time.After(time.Second):
			t.Fatal("resync left its stream open")
		}
	}
	profilesAfter, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(profilesBefore, profilesAfter) || retained() != before || streams.Load() != 2 || writes.Load() != 0 || f.resolverHit.Load() != 1 {
		t.Fatal("resync/replay changed retained state, profiles, or performed hidden work", err)
	}
}
