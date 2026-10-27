package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/server"
)

func waitPage(cursor string, items ...client.MessageEvent) client.MessageEventPage {
	if items == nil {
		items = []client.MessageEvent{}
	}
	return client.MessageEventPage{Items: items, Cursor: cursor}
}

func waitEvent(sequence int64, kind string) client.MessageEvent {
	value := client.MessageEvent{Sequence: sequence, ProjectID: "project-one", Type: kind, ResourceID: "message-one", Revision: 1, ActorID: "principal-one", Payload: client.MessageEventPayload{ConversationID: "conversation-one", RecipientWorkerID: "worker-one"}, CreatedAt: "2026-10-15T12:30:00Z"}
	if kind == "message.sent" {
		sender := "sender-one"
		value.Payload.SenderWorkerID = &sender
	}
	return value
}

func waitFrame(page client.MessageEventPage) string {
	data, _ := json.Marshal(page)
	return "event: message-events\nid: " + page.Cursor + "\ndata: " + string(data) + "\n\n"
}

func waitServer(t *testing.T, serve func(http.ResponseWriter, *http.Request)) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		serve(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestMessageWaitCRReadinessDoesNotNeedAnotherByte(t *testing.T) {
	s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, strings.ReplaceAll(waitFrame(waitPage("next", waitEvent(7, "message.sent"))), "\n", "\r"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	c := newClient(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	page, err := c.WaitForMessages(ctx, "cursor", 1, 1)
	if err != nil || len(page.Items) != 1 || page.Cursor != "next" {
		t.Fatalf("complete CR-terminated frame was delayed: page=%+v err=%v", page, err)
	}
}

func TestMessageWaitFiltersHandlingAndReturnsFirstRelevantPage(t *testing.T) {
	var streams atomic.Int64
	closed := make(chan struct{})
	s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
		streams.Add(1)
		if len(r.Header.Get("X-FC-Session")) != 32 {
			t.Error("quiet stream lost client connection identity")
		}
		defer close(closed)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/project-one/message-events/stream" || r.URL.Query().Get("cursor") != "cursor & literal" || r.URL.Query().Get("limit") != "20" || r.URL.Query().Get("include_assignments") != "true" || len(r.URL.Query()) != 3 || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("Accept") != "text/event-stream" {
			t.Error("wait request did not preserve explicit scope, defaults and selected route")
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = fmt.Fprint(w, ": keepalive\r\n\r\n", waitFrame(waitPage("ready")), waitFrame(waitPage("handling", waitEvent(3, "message.seen"))))
		page := waitPage("delivered", waitEvent(5, "message.acknowledged"), waitEvent(8, "message.sent"))
		page.HasMore = true
		_, _ = fmt.Fprint(w, waitFrame(page))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	c := newClient(t, s)
	page, err := c.WaitForMessages(context.Background(), "cursor & literal", 0, 0)
	if err != nil || len(page.Items) != 1 || page.Items[0].Sequence != 8 || page.Cursor != "delivered" || !page.HasMore || page.TimedOut || streams.Load() != 1 {
		t.Fatalf("first relevant page=%+v err=%v streams=%d", page, err, streams.Load())
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("successful wait retained its stream")
	}
}

func TestMessageWaitExplicitUpperBoundsAndCursorBytes(t *testing.T) {
	cursor := strings.Repeat("é", 2048) // The opaque cursor bound is bytes, not runes.
	s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") != cursor || r.URL.Query().Get("limit") != "100" {
			t.Error("explicit upper bounds did not reach the selected stream")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, waitFrame(waitPage("next", waitEvent(7, "message.sent"))))
	})
	c := newClient(t, s)
	page, err := c.WaitForMessages(t.Context(), cursor, 100, 60)
	if err != nil || len(page.Items) != 1 || page.Cursor != "next" || page.TimedOut {
		t.Fatalf("valid boundary refused: page=%+v err=%v", page, err)
	}
}

func TestMessageWaitQuietTimeoutUsesLastValidatedCursor(t *testing.T) {
	closed := make(chan struct{})
	s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(closed)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, waitFrame(waitPage("ready")), ": keepalive\n\n", waitFrame(waitPage("handled", waitEvent(10, "message.seen"))))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	c := newClient(t, s)
	start := time.Now()
	page, err := c.WaitForMessages(context.Background(), "original", 3, 1)
	if err != nil || page.Items == nil || len(page.Items) != 0 || !page.TimedOut || page.HasMore || page.Cursor != "handled" || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("quiet timeout=%+v err=%v elapsed=%s", page, err, time.Since(start))
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("expired wait retained its stream")
	}
}

func TestMessageWaitRejectsInvalidArgumentsBeforeHTTP(t *testing.T) {
	var calls atomic.Int64
	s := waitServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	c := newClient(t, s)
	for _, tc := range []struct {
		cursor      string
		limit, wait int
	}{{"", 1, 1}, {" cursor", 1, 1}, {"c\n", 1, 1}, {strings.Repeat("x", 4097), 1, 1}, {"c", -1, 1}, {"c", 101, 1}, {"c", 1, -1}, {"c", 1, 61}} {
		_, err := c.WaitForMessages(context.Background(), tc.cursor, tc.limit, tc.wait)
		checkError(t, err, "validation_failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.WaitForMessages(ctx, "cursor", 1, 1)
	checkError(t, err, "canceled")
	if calls.Load() != 0 {
		t.Fatal("invalid/canceled wait performed HTTP")
	}
}

func TestMessageWaitMalformedStreamsReturnNoPartialPage(t *testing.T) {
	valid := waitFrame(waitPage("next", waitEvent(7, "message.sent")))
	cases := map[string]string{
		"cursor_mismatch":         strings.Replace(valid, "id: next", "id: other", 1),
		"missing_id":              strings.Replace(valid, "id: next\n", "", 1),
		"duplicate_id":            strings.Replace(valid, "id: next", "id: next\nid: next", 1),
		"duplicate_event":         "event: message-events\n" + valid,
		"unsupported_field":       "retry: 1000\n" + valid,
		"unsupported_event":       strings.Replace(valid, "event: message-events", "event: message", 1),
		"duplicate_json":          strings.Replace(valid, `"sequence":7`, `"sequence":7,"sequence":8`, 1),
		"unknown_json":            strings.Replace(valid, `"sequence":7`, `"body":"unrequested","sequence":7`, 1),
		"null_items":              strings.Replace(waitFrame(waitPage("next")), `"items":[]`, `"items":null`, 1),
		"missing_timeout":         strings.Replace(valid, `,"timed_out":false`, "", 1),
		"null_sender":             strings.Replace(valid, `"sender_worker_id":"sender-one"`, `"sender_worker_id":null`, 1),
		"wrong_project":           strings.Replace(valid, "project-one", "project-other", 1),
		"wrong_recipient":         strings.Replace(valid, "worker-one", "worker-other", 1),
		"unsupported_item":        strings.Replace(valid, "message.sent", "job.created", 1),
		"bad_timestamp":           strings.Replace(valid, "2026-10-15T12:30:00Z", "not-a-time", 1),
		"server_timeout":          strings.Replace(valid, `"timed_out":false`, `"timed_out":true`, 1),
		"empty_backlog":           strings.Replace(waitFrame(waitPage("next")), `"has_more":false`, `"has_more":true`, 1),
		"regressed_across_pages":  waitFrame(waitPage("handling", waitEvent(8, "message.seen"))) + valid,
		"duplicate_across_pages":  waitFrame(waitPage("handling", waitEvent(7, "message.seen"))) + valid,
		"invalid_before_valid":    waitFrame(waitPage("handling", waitEvent(8, "job.created"))) + valid,
		"invalid_utf8_comment":    ": \xff\n\n" + valid,
		"oversized_comment_frame": ":" + strings.Repeat("x", 1<<20) + "\n\n" + valid,
		"aggregate_comments":      strings.Repeat(":"+strings.Repeat("x", (1<<20)-4)+"\n\n", 5) + valid,
		"too_many_items":          waitFrame(waitPage("next", waitEvent(7, "message.sent"), waitEvent(8, "message.sent"))),
		"error_with_id":           "event: error\nid: next\ndata: {}\n\n",
	}
	for name, stream := range cases {
		t.Run(name, func(t *testing.T) {
			s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, stream)
			})
			c := newClient(t, s)
			page, err := c.WaitForMessages(context.Background(), "original", 1, 1)
			checkError(t, err, "invalid_response")
			if page.Items != nil || page.Cursor != "" {
				t.Fatal("failed stream exposed a partial page or advanced cursor")
			}
		})
	}
}

func TestMessageWaitNonJSONServerErrorsRetainStatusAndCursorRecovery(t *testing.T) {
	for _, status := range []int{200, 403, 502, 503, 504} {
		for _, media := range []string{"text/html", "invalid;=", "", "encoded_json"} {
			t.Run(http.StatusText(status)+"/"+media, func(t *testing.T) {
				var calls atomic.Int64
				s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/project-one/message-events/stream" || r.URL.RawQuery != "cursor=saved-cursor&include_assignments=true&limit=5" {
						t.Error("wait selection changed")
					}
					w.Header().Set("Content-Type", media)
					if media == "" {
						w.Header()["Content-Type"] = nil
					} else if media == "encoded_json" {
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("Content-Encoding", "br")
					}
					w.Header().Set("X-Request-ID", "PRIVATE_PROXY_REQUEST")
					w.WriteHeader(status)
					_, _ = fmt.Fprint(w, "<html>PRIVATE_PROXY_BODY ", testToken, "</html>")
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				})
				c := newClient(t, s)
				page, err := c.WaitForMessages(t.Context(), "saved-cursor", 5, 1)
				wantCode, wantStatus := "invalid_response", 0
				if status >= 500 {
					wantCode, wantStatus = "service_unavailable", status
				}
				checkError(t, err, wantCode, "PRIVATE_PROXY_BODY", "PRIVATE_PROXY_REQUEST")
				var failure *client.Error
				if !errors.As(err, &failure) || failure.Status != wantStatus || failure.OutcomeUnknown || failure.RequestID != "" || failure.Details != nil || calls.Load() != 1 || page.Items != nil || page.Cursor != "" || page.TimedOut || page.HasMore {
					t.Fatalf("wait error classification, cursor or no-retry changed: %#v page=%+v calls=%d", failure, page, calls.Load())
				}
				if status >= 500 && (!strings.Contains(failure.Action, "saved cursor") || !strings.Contains(failure.Action, "inbox") || strings.Contains(failure.Action, "compatibility")) {
					t.Fatal("server outage lost saved-cursor recovery guidance")
				}
			})
		}
	}
}

func TestMessageWaitPreservesSafeServiceErrorsAndRejectsTransportShape(t *testing.T) {
	failure := `{"error":{"code":"resync_required","message":"Get a scoped snapshot."},"request_id":"request-one"}`
	for _, tc := range []struct {
		name, media, body, encoding, code string
		status                            int
	}{
		{"http_error", "application/json", failure, "", "resync_required", 409},
		{"http_resync_with_actual_status", "application/json", failure, "", "resync_required", 503},
		{"http_forbidden", "application/json", strings.Replace(failure, "resync_required", "forbidden", 1), "", "forbidden", 403},
		{"instance_changed", "application/json", strings.Replace(failure, "resync_required", "instance_mismatch", 1), "", "instance_mismatch", 409},
		{"stream_error", "text/event-stream", waitFrame(waitPage("ready")) + "event: error\ndata: " + failure + "\n\n", "", "resync_required", 200},
		{"secret_error", "text/event-stream", "event: error\ndata: " + strings.Replace(failure, "Get a scoped snapshot.", testToken, 1) + "\n\n", "", "invalid_response", 200},
		{"wrong_media", "application/json", "{}", "", "invalid_response", 200},
		{"encoded", "text/event-stream", waitFrame(waitPage("next", waitEvent(7, "message.sent"))), "gzip", "invalid_response", 200},
		{"redirect", "text/event-stream", "", "", "redirect_rejected", 302},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", tc.media)
				w.Header().Set("Content-Encoding", tc.encoding)
				w.Header().Set("Location", "/redirect-target")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			})
			c := newClient(t, s)
			_, err := c.WaitForMessages(context.Background(), "cursor", 1, 1)
			checkError(t, err, tc.code)
			if tc.code == "resync_required" {
				var failure *client.Error
				wantStatus := tc.status
				if tc.media == "text/event-stream" {
					wantStatus = http.StatusConflict
				}
				if !errors.As(err, &failure) || failure.Status != wantStatus || failure.RequestID != "request-one" || failure.Action != "Read a fresh bound-worker inbox snapshot, then continue from its event cursor; pending messages remain unacknowledged." || strings.Contains(failure.Message, "Get a scoped snapshot.") {
					t.Fatalf("resync action/request identity was lost or provider text reflected: %v", err)
				}
			}
			if calls.Load() != 1 {
				t.Fatal("wait followed redirect or retried after failure")
			}
		})
	}
}

func TestMessageWaitCallerCancellationAndReadinessDeadline(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprintf("ready_%t", ready), func(t *testing.T) {
			connected, closed := make(chan struct{}), make(chan struct{})
			s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
				defer close(closed)
				w.Header().Set("Content-Type", "text/event-stream")
				if ready {
					_, _ = fmt.Fprint(w, waitFrame(waitPage("ready")))
				}
				w.(http.Flusher).Flush()
				close(connected)
				<-r.Context().Done()
			})
			c := newClient(t, s)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { _, err := c.WaitForMessages(ctx, "cursor", 1, 1); done <- err }()
			<-connected
			cancel()
			select {
			case err := <-done:
				checkError(t, err, "canceled")
			case <-time.After(time.Second):
				t.Fatal("canceled wait did not return promptly")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("canceled wait did not close its stream")
			}
			// A second independent call uses a fresh server, avoiding reuse of the
			// one-shot fixture channels above.
			blocked := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
				if ready {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, waitFrame(waitPage("ready")))
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			})
			other := newClient(t, blocked)
			ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			_, err := other.WaitForMessages(ctx, "cursor", 1, 1)
			checkError(t, err, "deadline_exceeded")
		})
	}
}

func TestMessageWaitPublicHandlerRetainsReplayWithoutAcknowledgment(t *testing.T) {
	f := newMutationFixture(t)
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	server.RegisterMessageStream(mux, f.store, context.Background())
	s := httptest.NewServer(mux)
	defer s.Close()
	c := f.newClient(t, f.admin, f.recipient, s.URL)
	snapshot, err := c.InboxSnapshot(context.Background(), client.PageOptions{}, client.InboxFilter{})
	if err != nil {
		t.Fatal(err)
	}
	message := f.call(t, http.MethodPost, "/api/v1/projects/"+f.project+"/messages", f.admin, f.sender, map[string]string{"recipient_worker_id": f.recipient, "body": "full body remains behind explicit conversation read"})
	before, providers := f.eventCount(t), f.providerCalls.Load()
	page, err := c.WaitForMessages(context.Background(), snapshot.EventCursor, 1, 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].ResourceID != message["id"] || page.TimedOut {
		t.Fatalf("public SSE wait=%+v err=%v receipt=%v", page, err, message)
	}
	replay, err := c.WaitForMessages(context.Background(), snapshot.EventCursor, 1, 1)
	if err != nil || len(replay.Items) != 1 || replay.Items[0].Sequence != page.Items[0].Sequence || replay.Cursor != page.Cursor {
		t.Fatalf("explicit replay=%+v err=%v", replay, err)
	}
	inbox, err := c.Inbox(context.Background(), client.PageOptions{}, client.InboxFilter{Unread: true, Unacknowledged: true})
	if err != nil || len(inbox.Items) != 1 || inbox.Items[0].Seen || inbox.Items[0].Acknowledged || f.eventCount(t) != before || f.providerCalls.Load() != providers {
		t.Fatalf("wait changed durable handling or provider state: inbox=%+v err=%v", inbox, err)
	}
}

func TestMessageWaitReadinessRequiresPageWithinFiveSeconds(t *testing.T) {
	s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, ": keepalive is not readiness\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	c := newClient(t, s)
	start := time.Now()
	_, err := c.WaitForMessages(context.Background(), "cursor", 1, 1)
	checkError(t, err, "deadline_exceeded")
	if elapsed := time.Since(start); elapsed < 4500*time.Millisecond || elapsed > 6*time.Second {
		t.Fatalf("readiness deadline was not separately bounded: %s", elapsed)
	}
}

func TestMessageWaitEstablishedStreamOutlivesNormalReadBudget(t *testing.T) {
	connected := make(chan struct{})
	s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stream") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, waitFrame(waitPage("ready")))
			w.(http.Flusher).Flush()
			close(connected)
		}
		<-r.Context().Done()
	})
	c := newClient(t, s)
	type result struct {
		page client.MessageEventPage
		err  error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		page, err := c.WaitForMessages(context.Background(), "cursor", 1, 6)
		done <- result{page, err}
	}()
	<-connected
	finiteStart := time.Now()
	_, err := c.Workers(context.Background(), client.PageOptions{})
	checkError(t, err, "deadline_exceeded")
	if elapsed := time.Since(finiteStart); elapsed < 4500*time.Millisecond || elapsed > 6*time.Second {
		t.Fatalf("wait changed the concurrent ordinary read deadline: %s", elapsed)
	}
	select {
	case result := <-done:
		if result.err != nil || !result.page.TimedOut || result.page.Cursor != "ready" || time.Since(start) < 5800*time.Millisecond {
			t.Fatalf("established wait was cut off by finite client timeout: %+v err=%v elapsed=%s", result.page, result.err, time.Since(start))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("established stream exceeded selected wait")
	}
}

func TestMessageWaitManyDataLinesRespectExactFrameBudget(t *testing.T) {
	// Empty SSE data lines become legal JSON whitespace. Many short lines must
	// remain linear in the wire bound, rather than repeatedly copying payloads.
	data, _ := json.Marshal(waitPage("next", waitEvent(7, "message.sent")))
	prefix := "event: message-events\nid: next\n"
	suffix := "data: " + string(data) + "\n\n"
	const bound = 1 << 20
	space := bound - len(prefix) - len(suffix)
	lines := space / len("data:\n")
	remainder := space % len("data:\n")
	frame := prefix + strings.Repeat("data:\n", lines)
	if remainder > 0 {
		// A bounded comment fills the exact remainder; use one fewer data line
		// when fewer than three bytes remain for the comment syntax.
		if remainder < 3 {
			frame = strings.TrimSuffix(frame, "data:\n")
			remainder += len("data:\n")
		}
		frame += ":" + strings.Repeat("x", remainder-2) + "\n"
	}
	frame += suffix
	if len(frame) != bound {
		t.Fatalf("fixture frame size=%d", len(frame))
	}
	for _, extra := range []bool{false, true} {
		t.Run(fmt.Sprintf("over_%t", extra), func(t *testing.T) {
			body := frame
			if extra {
				body = ":x\n" + frame
			}
			s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, body)
			})
			c := newClient(t, s)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			page, err := c.WaitForMessages(ctx, "cursor", 1, 1)
			if extra {
				checkError(t, err, "invalid_response")
			} else if err != nil || len(page.Items) != 1 {
				t.Fatalf("bounded many-line frame failed: page=%+v err=%v", page, err)
			}
		})
	}
}
