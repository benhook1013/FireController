package server_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
)

const eventClientToken = "synthetic-message-events-client-token"

type messageSnapshot struct {
	EventCursor string `json:"event_cursor"`
}

type messageEventPage struct {
	Items    []core.Event `json:"items"`
	Cursor   string       `json:"cursor"`
	HasMore  bool         `json:"has_more"`
	TimedOut bool         `json:"timed_out"`
}

type messageReceipt struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	Outcome        string `json:"outcome"`
}

func registerMessages(t *testing.T, f *fixture) {
	t.Helper()
	if err := inbox.EnsureSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	mux, ok := f.handler.(*http.ServeMux)
	if !ok {
		t.Fatal("server fixture mux unavailable")
	}
	inbox.Register(mux, f.store)
}

func messageSetup(t *testing.T) (*fixture, string, string, string, string) {
	t.Helper()
	f := setup(t)
	registerMessages(t, f)
	project := f.project(t, "owner/events")
	sender := f.worker(t, project, "Builder")
	recipient := f.worker(t, project, "Overseer")
	other := f.worker(t, project, "Reviewer")
	f.grant(t, project, eventClientToken, true)
	return f, project, sender, recipient, other
}

func snapshotRequest(t *testing.T, f *fixture, project, worker string) string {
	t.Helper()
	return snapshotCursorFor(t, f, project, worker, eventClientToken)
}

func snapshotCursorFor(t *testing.T, f *fixture, project, worker, token string) string {
	t.Helper()
	path := "/api/v1/projects/" + project + "/inbox/" + worker + "/snapshot"
	value := require(t, request(f.handler, http.MethodGet, path, token, "", worker, ""), http.StatusOK)
	inboxValue, ok := value["inbox"].(map[string]any)
	if !ok || inboxValue["items"] == nil {
		t.Fatalf("snapshot omitted compact inbox page: %#v", value)
	}
	cursor, ok := value["event_cursor"].(string)
	if !ok || cursor == "" {
		t.Fatalf("snapshot omitted event cursor: %#v", value)
	}
	return cursor
}

func sendMessage(t *testing.T, f *fixture, project, sender, recipient, body, key string) messageReceipt {
	t.Helper()
	path := "/api/v1/projects/" + project + "/messages"
	w := request(f.handler, http.MethodPost, path, eventClientToken, key, sender, `{"recipient_worker_id":"`+recipient+`","body":"`+body+`"}`)
	value := require(t, w, http.StatusCreated)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var receipt messageReceipt
	if err := json.Unmarshal(encoded, &receipt); err != nil || receipt.ID == "" || receipt.ConversationID == "" || receipt.Outcome != "sent" {
		t.Fatalf("invalid message receipt: %s (%v)", encoded, err)
	}
	if strings.Contains(w.Body.String(), body) {
		t.Fatal("message receipt exposed body")
	}
	return receipt
}

func eventsRequest(t *testing.T, f *fixture, project, worker, cursor string, limit, waitMS int) (int, messageEventPage) {
	t.Helper()
	query := url.Values{"cursor": {cursor}, "limit": {strconv.Itoa(limit)}, "wait_ms": {strconv.Itoa(waitMS)}}
	path := "/api/v1/projects/" + project + "/message-events?" + query.Encode()
	w := request(f.handler, http.MethodGet, path, eventClientToken, "", worker, "")
	if w.Code >= 500 {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.SetPathValue("project_id", project)
		r.Header.Set("Authorization", "Bearer "+eventClientToken)
		r.Header.Set("X-FC-Worker", worker)
		scope, scopeErr := f.store.Scope(r, "read")
		if scopeErr == nil {
			_, scopeErr = f.store.MessageEvents(r.Context(), scope, cursor, limit, time.Duration(waitMS)*time.Millisecond)
		}
		t.Fatalf("event endpoint HTTP %d, direct error: %v", w.Code, scopeErr)
	}
	var page messageEventPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("invalid event response HTTP %d: %s (%v)", w.Code, w.Body.String(), err)
	}
	return w.Code, page
}

func TestMessageEventsReplayFiltersGapsPaginatesAndExpires(t *testing.T) {
	f, project, sender, recipient, other := messageSetup(t)
	initial := snapshotRequest(t, f, project, recipient)
	wrongWorkerStatus, _ := eventsRequest(t, f, project, other, initial, 10, 0)
	if wrongWorkerStatus != http.StatusBadRequest {
		t.Fatalf("worker-bound cursor accepted for another worker: HTTP %d", wrongWorkerStatus)
	}
	// Advance the project sequence with a message for another worker. A
	// nonmatching event is a gap in this worker's message feed, not a payload.
	sendMessage(t, f, project, sender, other, "private for other", "event-other-1")
	status, skipped := eventsRequest(t, f, project, recipient, initial, 10, 0)
	if status != http.StatusOK || len(skipped.Items) != 0 || skipped.TimedOut || skipped.Cursor == initial {
		t.Fatalf("filtered gap page HTTP %d: %+v", status, skipped)
	}
	first := sendMessage(t, f, project, sender, recipient, "private first", "event-recipient-1")
	second := sendMessage(t, f, project, sender, recipient, "private second", "event-recipient-2")
	status, one := eventsRequest(t, f, project, recipient, skipped.Cursor, 1, 0)
	if status != http.StatusOK || len(one.Items) != 1 || one.Items[0].ResourceID != first.ID || !one.HasMore || one.TimedOut || one.Cursor == skipped.Cursor {
		t.Fatalf("first page HTTP %d: %+v", status, one)
	}
	status, two := eventsRequest(t, f, project, recipient, one.Cursor, 1, 0)
	if status != http.StatusOK || len(two.Items) != 1 || two.Items[0].ResourceID != second.ID || two.HasMore || two.TimedOut {
		t.Fatalf("second page HTTP %d: %+v", status, two)
	}
	if got := two.Items[0].Payload; !strings.Contains(string(got), recipient) || strings.Contains(string(got), "private second") {
		t.Fatalf("event payload not compact/recipient scoped: %s", got)
	}
	if _, err := f.store.DB().Exec("UPDATE events SET created_at=? WHERE project_id=?", time.Now().UTC().Add(-8*24*time.Hour).Format(time.RFC3339Nano), project); err != nil {
		t.Fatal(err)
	}
	status, expired := eventsRequest(t, f, project, recipient, initial, 10, 0)
	if status != http.StatusConflict || len(expired.Items) != 0 {
		t.Fatalf("expired cursor HTTP %d: %+v", status, expired)
	}
	if f.calls.Load() != 1 {
		t.Fatal("message event replay called repository provider")
	}
}

func TestSnapshotWatermarkSharesReadTransactionWithInboxView(t *testing.T) {
	f, project, sender, recipient, _ := messageSetup(t)
	requestScope := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+project+"/inbox/"+recipient+"/snapshot", nil)
	requestScope.SetPathValue("project_id", project)
	requestScope.Header.Set("Authorization", "Bearer "+eventClientToken)
	requestScope.Header.Set("X-FC-Worker", recipient)
	scope, err := f.store.Scope(requestScope, "read")
	if err != nil {
		t.Fatal(err)
	}
	type sendResult struct {
		receipt messageReceipt
		status  int
		body    string
	}
	sendStarted := make(chan struct{})
	sendFinished := make(chan sendResult, 1)
	var committed sendResult
	go func() {
		<-sendStarted
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project+"/messages", strings.NewReader(`{"recipient_worker_id":"`+recipient+`","body":"between snapshot and watermark"}`)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+eventClientToken)
		r.Header.Set("X-FC-Worker", sender)
		r.Header.Set("Idempotency-Key", "snapshot-interleaving-send")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		result := sendResult{status: w.Code, body: w.Body.String()}
		if w.Code == http.StatusCreated {
			_ = json.Unmarshal(w.Body.Bytes(), &result.receipt)
		}
		sendFinished <- result
	}()

	var cursor string
	var inboxBefore, inboxAfter int
	err = f.store.Read(context.Background(), scope, func(ctx context.Context, tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM inbox_messages WHERE project_id=? AND recipient_worker_id=?", project, recipient).Scan(&inboxBefore); err != nil {
			return err
		}
		close(sendStarted)
		// The separate WAL writer must commit while this read transaction stays
		// open. Taking the watermark afterward must still observe this tx's old
		// event high-water, paired with the inbox view above.
		select {
		case result := <-sendFinished:
			if result.status != http.StatusCreated || result.receipt.ID == "" {
				return fmt.Errorf("public send did not commit during read snapshot: HTTP %d %s", result.status, result.body)
			}
			committed = result
		case <-ctx.Done():
			return fmt.Errorf("public WAL writer did not commit while read snapshot remained open: %w", ctx.Err())
		case <-time.After(2 * time.Second):
			return fmt.Errorf("public WAL writer did not commit while read snapshot remained open")
		}
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM inbox_messages WHERE project_id=? AND recipient_worker_id=?", project, recipient).Scan(&inboxAfter); err != nil {
			return err
		}
		if inboxAfter != inboxBefore {
			return fmt.Errorf("inbox projection changed within pinned snapshot: before=%d after=%d", inboxBefore, inboxAfter)
		}
		cursor, err = f.store.MessageEventCursor(ctx, tx, scope)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	result := committed
	if result.receipt.ID == "" || cursor == "" {
		t.Fatalf("missing interleaved send or snapshot cursor: %+v cursor=%q", result, cursor)
	}
	position, err := f.store.ParseCursor(scope, "events:incoming-messages:"+recipient, cursor)
	if err != nil {
		t.Fatal(err)
	}
	watermark, err := strconv.ParseInt(position, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	var sendSequence int64
	if err := f.store.DB().QueryRow("SELECT sequence FROM events WHERE project_id=? AND resource_id=? AND event_type='message.sent'", project, result.receipt.ID).Scan(&sendSequence); err != nil {
		t.Fatal(err)
	}
	if watermark >= sendSequence {
		t.Fatalf("snapshot watermark %d included concurrent event sequence %d", watermark, sendSequence)
	}
	status, replay := eventsRequest(t, f, project, recipient, cursor, 10, 0)
	if status != http.StatusOK || len(replay.Items) != 1 || replay.Items[0].ResourceID != result.receipt.ID {
		t.Fatalf("concurrent message was not replayable after snapshot HTTP %d: %+v", status, replay)
	}
}

func TestMessageEventsWaitCatchesCommitAndReplayHasNoSecondEffect(t *testing.T) {
	f, project, sender, recipient, _ := messageSetup(t)
	cursor := snapshotRequest(t, f, project, recipient)
	path := "/api/v1/projects/" + project + "/message-events?cursor=" + url.QueryEscape(cursor) + "&limit=10&wait_ms=1500"
	// Start and commit without waiting for a registration handshake. This
	// exercises both possible orderings: notification after registration or
	// the initial durable read observing the already-committed event.
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+eventClientToken)
		r.Header.Set("X-FC-Worker", recipient)
		f.handler.ServeHTTP(w, r)
		result <- w
	}()
	receipt := sendMessage(t, f, project, sender, recipient, "committed once", "event-exact-replay")
	select {
	case w := <-result:
		var page messageEventPage
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != http.StatusOK || len(page.Items) != 1 || page.Items[0].ResourceID != receipt.ID || page.TimedOut {
			t.Fatalf("wait did not deliver commit HTTP %d: %+v %s (%v)", w.Code, page, w.Body.String(), err)
		}
		cursor = page.Cursor
	case <-time.After(2 * time.Second):
		t.Fatal("message wait did not finish after committed send")
	}
	before := count(t, f.store, "events")
	// Exact replay returns the old receipt and must neither append an event nor
	// wake a waiter with a duplicate message.
	path = "/api/v1/projects/" + project + "/message-events?cursor=" + url.QueryEscape(cursor) + "&limit=10&wait_ms=100"
	result = make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+eventClientToken)
		r.Header.Set("X-FC-Worker", recipient)
		f.handler.ServeHTTP(w, r)
		result <- w
	}()
	replayed := sendMessage(t, f, project, sender, recipient, "committed once", "event-exact-replay")
	if replayed.ID != receipt.ID || count(t, f.store, "events") != before {
		t.Fatal("exact replay duplicated durable message or event")
	}
	select {
	case w := <-result:
		var page messageEventPage
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != http.StatusOK || len(page.Items) != 0 || !page.TimedOut {
			t.Fatalf("exact replay woke message wait HTTP %d: %+v %s (%v)", w.Code, page, w.Body.String(), err)
		}
	case <-time.After(time.Second):
		t.Fatal("replay wait did not expire")
	}
}

func TestMessageEventsWaitCancellationUnrelatedWorkerAndRevocation(t *testing.T) {
	f, project, sender, recipient, other := messageSetup(t)
	cursor := snapshotRequest(t, f, project, recipient)
	status, page := eventsRequest(t, f, project, recipient, cursor, 10, 150)
	if status != http.StatusOK || !page.TimedOut || len(page.Items) != 0 {
		t.Fatalf("idle wait HTTP %d: %+v", status, page)
	}
	// An unrelated recipient does not wake this worker's feed; the bounded
	// timeout may still advance over that project-wide sequence gap.
	cursor = snapshotRequest(t, f, project, recipient)
	path := "/api/v1/projects/" + project + "/message-events?cursor=" + url.QueryEscape(cursor) + "&limit=10&wait_ms=150"
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+eventClientToken)
		r.Header.Set("X-FC-Worker", recipient)
		f.handler.ServeHTTP(w, r)
		result <- w
	}()
	_ = sendMessage(t, f, project, sender, other, "unrelated", "event-unrelated")
	select {
	case w := <-result:
		var got messageEventPage
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || len(got.Items) != 0 || !got.TimedOut {
			t.Fatalf("unrelated message woke worker wait HTTP %d: %+v %s (%v)", w.Code, got, w.Body.String(), err)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated-worker wait did not time out")
	}

	// Caller cancellation releases the in-memory waiter promptly.
	ctx, cancel := context.WithCancel(context.Background())
	cancelPath := "/api/v1/projects/" + project + "/message-events?cursor=" + url.QueryEscape(cursor) + "&limit=10&wait_ms=4000"
	canceled := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, cancelPath, nil).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+eventClientToken)
		r.Header.Set("X-FC-Worker", recipient)
		f.handler.ServeHTTP(w, r)
		canceled <- w
	}()
	cancel()
	select {
	case w := <-canceled:
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("canceled wait status=%d body=%s", w.Code, w.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("canceled wait retained request")
	}

	// Revocation notifies waiters globally; the next read reauthorizes inside
	// its transaction and returns no event data.
	const revocationToken = "synthetic-revocation-client-token"
	credentialID := f.grant(t, project, revocationToken, true)
	revocationCursor := snapshotCursorFor(t, f, project, recipient, revocationToken)
	revokeWaitPath := "/api/v1/projects/" + project + "/message-events?cursor=" + url.QueryEscape(revocationCursor) + "&limit=10&wait_ms=4000"
	revoked := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, revokeWaitPath, nil)
		r.Header.Set("Authorization", "Bearer "+revocationToken)
		r.Header.Set("X-FC-Worker", recipient)
		f.handler.ServeHTTP(w, r)
		revoked <- w
	}()
	adminPath := "/api/v1/projects/" + project + "/credentials/" + credentialID
	require(t, request(f.handler, http.MethodDelete, adminPath, f.admin, core.NewID("revoke"), "", ""), http.StatusOK)
	select {
	case w := <-revoked:
		if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
			t.Fatalf("revoked waiter HTTP %d: %s", w.Code, w.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("credential revocation did not release waiter")
	}
}

func TestMessageEventWaitCapacityReleasesCanceledPrincipalSlot(t *testing.T) {
	f, project, _, recipient, _ := messageSetup(t)
	cursor := snapshotRequest(t, f, project, recipient)
	path := "/api/v1/projects/" + project + "/message-events?cursor=" + url.QueryEscape(cursor) + "&wait_ms=4000"
	type completed struct {
		index    int
		response *httptest.ResponseRecorder
	}
	done := make(chan completed, 17)
	cancels := make([]context.CancelFunc, 17)
	for i := range cancels {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		go func(index int) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+eventClientToken)
			r.Header.Set("X-FC-Worker", recipient)
			f.handler.ServeHTTP(w, r)
			done <- completed{index, w}
		}(i)
	}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	// Seventeen long-lived callers establish a full sixteen-slot pool and one
	// rejection without an admission probe stealing a slot from setup callers.
	var rejected completed
	select {
	case rejected = <-done:
		if rejected.response.Code != http.StatusServiceUnavailable || !strings.Contains(rejected.response.Body.String(), "bounded event wait capacity") {
			t.Fatalf("seventeenth waiter response=%d %s", rejected.response.Code, rejected.response.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("principal wait capacity was not enforced")
	}
	canceledIndex := 0
	if rejected.index == 0 {
		canceledIndex = 1
	}
	cancels[canceledIndex]()
	select {
	case released := <-done:
		if released.index != canceledIndex || released.response.Code != http.StatusServiceUnavailable {
			t.Fatalf("unexpected canceled wait: %+v", released)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not release the request")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+eventClientToken)
	r.Header.Set("X-FC-Worker", recipient)
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "bounded event wait capacity") {
		t.Fatalf("replacement did not acquire released slot: %d %s", w.Code, w.Body.String())
	}
	for _, cancel := range cancels {
		cancel()
	}
	for remaining := 15; remaining > 0; remaining-- {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("waiter retained after cancellation")
		}
	}
}

func TestFailedMessageTransactionDoesNotNotifyOrAdvanceFeed(t *testing.T) {
	f, project, sender, recipient, _ := messageSetup(t)
	cursor := snapshotRequest(t, f, project, recipient)
	before := count(t, f.store, "events")
	if _, err := f.store.DB().Exec("CREATE TRIGGER fail_message_event BEFORE INSERT ON events WHEN NEW.event_type='message.sent' BEGIN SELECT RAISE(ABORT,'synthetic event failure'); END"); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/projects/" + project + "/message-events?cursor=" + url.QueryEscape(cursor) + "&limit=10&wait_ms=100"
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+eventClientToken)
		r.Header.Set("X-FC-Worker", recipient)
		f.handler.ServeHTTP(w, r)
		result <- w
	}()
	failed := request(f.handler, http.MethodPost, "/api/v1/projects/"+project+"/messages", eventClientToken, "failed-message-key", sender, `{"recipient_worker_id":"`+recipient+`","body":"transaction must roll back"}`)
	if failed.Code < 500 {
		t.Fatalf("triggered transaction failure HTTP %d: %s", failed.Code, failed.Body.String())
	}
	select {
	case w := <-result:
		var page messageEventPage
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != http.StatusOK || len(page.Items) != 0 || !page.TimedOut {
			t.Fatalf("failed transaction woke feed HTTP %d: %+v %s (%v)", w.Code, page, w.Body.String(), err)
		}
	case <-time.After(time.Second):
		t.Fatal("failed transaction wait did not expire")
	}
	if count(t, f.store, "events") != before {
		t.Fatal("failed message transaction persisted an event")
	}
	if _, err := f.store.DB().Exec("DROP TRIGGER fail_message_event"); err != nil {
		t.Fatal(err)
	}
	sent := sendMessage(t, f, project, sender, recipient, "transaction must roll back", "failed-message-key")
	status, page := eventsRequest(t, f, project, recipient, cursor, 10, 0)
	if status != http.StatusOK || len(page.Items) != 1 || page.Items[0].ResourceID != sent.ID || count(t, f.store, "events") != before+1 {
		t.Fatalf("post-rollback retry failed exactly once HTTP %d: %+v", status, page)
	}
}
