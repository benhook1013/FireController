package historycollect

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func collectionWaitCount(t *testing.T, e *Engine, want int) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		e.waitMu.Lock()
		count := e.waitCount
		e.waitMu.Unlock()
		if count == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("waiter count=%d want=%d", count, want)
		case <-tick.C:
		}
	}
}

type collectionWaitResult struct {
	operation Operation
	err       error
}

func TestCollectionWaitBoundedExpiryIsReadOnly(t *testing.T) {
	f := newCollectionStoreFixture(t)
	project, worker := f.project(t, "selected/repository")
	scope := f.scope(t, project, worker, f.admin)
	e := f.engine(t)
	receipt := f.enqueue(t, scope, "bounded-wait", Selection{[]int64{47}})
	before := collectionStoreRows(t, f)
	got, err := e.Wait(t.Context(), scope, receipt.ID, 10*time.Millisecond)
	if err != nil || got.State != "queued" || got.Revision != 1 || before != collectionStoreRows(t, f) || len(f.provider.calls) != 0 {
		t.Fatal("wait expiry invented progress or altered retained state", err)
	}
	collectionWaitCount(t, e, 0)
}

func TestCollectionWaitSubscribeBeforeFirstReadAndCommittedWake(t *testing.T) {
	f := newCollectionStoreFixture(t)
	project, worker := f.project(t, "selected/repository")
	scope := f.scope(t, project, worker, f.admin)
	e := f.engine(t)
	receipt := f.enqueue(t, scope, "wait-race", Selection{[]int64{47}})
	// Occupy the read pool after obtaining real authority. Wait can subscribe,
	// but its initial authorized read cannot proceed until after Change commits
	// on the separate real writer connection and publishes its notification.
	f.store.DB().SetMaxOpenConns(1)
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	result := make(chan collectionWaitResult, 1)
	go func() {
		o, err := e.Wait(t.Context(), scope, receipt.ID, time.Second)
		result <- collectionWaitResult{o, err}
	}()
	collectionWaitCount(t, e, 1)
	if _, err := e.Change(t.Context(), scope, receipt.ID, "commit-while-read-held", "cancel", 1); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || got.operation.State != "canceled" || got.operation.Revision != 2 {
			t.Fatal("commit around first read was missed", got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("subscribed completion failed to return")
	}
	collectionWaitCount(t, e, 0)
	before := collectionStoreRows(t, f)
	if got, err := e.Wait(t.Context(), scope, receipt.ID, time.Second); err != nil || got.State != "canceled" || collectionStoreRows(t, f) != before {
		t.Fatal("already committed terminal read changed state", err)
	}
}

func TestCollectionWaitCancellationAndRevocationDetachWithoutActionChange(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		name := "cancellation"
		if revoke {
			name = "revocation"
		}
		t.Run(name, func(t *testing.T) {
			f := newCollectionStoreFixture(t)
			project, worker := f.project(t, "selected/repository")
			scope := f.scope(t, project, worker, f.admin)
			e := f.engine(t)
			receipt := f.enqueue(t, scope, "wait-auth", Selection{[]int64{47}})
			before := collectionStoreRows(t, f)
			f.store.DB().SetMaxOpenConns(1)
			conn, err := f.store.DB().Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan collectionWaitResult, 1)
			go func() {
				o, err := e.Wait(ctx, scope, receipt.ID, 5*time.Second)
				result <- collectionWaitResult{o, err}
			}()
			collectionWaitCount(t, e, 1)
			if revoke {
				if _, err := conn.ExecContext(t.Context(), `UPDATE credentials SET revoked=1 WHERE is_admin=1`); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-result:
				want := "overloaded"
				if revoke {
					want = "unauthorized"
				}
				collectionStoreCode(t, got.err, want)
				if got.operation.ID != "" {
					t.Fatal("failed waiter exposed a terminal result")
				}
			case <-time.After(time.Second):
				t.Fatal("canceled or revoked waiter did not detach")
			}
			collectionWaitCount(t, e, 0)
			if before != collectionStoreRows(t, f) || len(f.provider.calls) != 0 {
				t.Fatal("wait cancellation or revocation changed the durable action")
			}
		})
	}
}

func TestCollectionHTTPExplicitWaitExtendsWriteBudgetAndCallerCancellationDetaches(t *testing.T) {
	h := newCollectionHTTPFixture(t, 25*time.Millisecond)
	body, _ := collectionHTTPDo(t, h.request(t, "POST", h.path(), "held", `{"pull_numbers":[47]}`, "wait=0"), 202)
	id := collectionHTTPReceipt(t, body).ID
	body, headers := collectionHTTPDo(t, h.request(t, "GET", h.path()+"/"+id, "", ""), 200)
	var immediate Operation
	if json.Unmarshal(body, &immediate) != nil || immediate.State != "queued" || headers.Get("Preference-Applied") != "wait=0" {
		t.Fatal("default GET unexpectedly changed or waited for the action")
	}
	ctx, cancel := context.WithCancel(t.Context())
	r := h.request(t, "GET", h.path()+"/"+id, "", "", "wait=900").WithContext(ctx)
	done := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(r)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	collectionWaitCount(t, h.e, 1)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("caller cancellation did not detach its HTTP call")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled long wait did not return")
	}
	collectionWaitCount(t, h.e, 0)
	scope := h.f.scope(t, h.project, h.worker, h.f.admin)
	if current := collectionStoreRead(t, h.e, scope, id); current.State != "queued" || current.Revision != 1 {
		t.Fatal("canceled result wait canceled the action")
	}
	// The exact same live GET waits beyond the server's original25ms write
	// deadline, then receives an actual postcommit terminal notification.
	responseDone := make(chan struct {
		status int
		body   []byte
		err    error
	}, 1)
	request := h.request(t, "GET", h.path()+"/"+id, "", "", "wait=1")
	go func() {
		response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
		if err != nil {
			responseDone <- struct {
				status int
				body   []byte
				err    error
			}{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		responseDone <- struct {
			status int
			body   []byte
			err    error
		}{response.StatusCode, body, err}
	}()
	collectionWaitCount(t, h.e, 1)
	timer := time.NewTimer(70 * time.Millisecond)
	<-timer.C
	if _, err := h.e.Change(t.Context(), scope, id, "finish-held", "cancel", 1); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-responseDone:
		var operation Operation
		if result.err != nil || result.status != 200 || json.Unmarshal(result.body, &operation) != nil || operation.State != "canceled" || operation.Revision != 2 {
			t.Fatal("explicit wait lost terminal response after original write deadline", result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal notification did not wake live HTTP wait")
	}
	collectionWaitCount(t, h.e, 0)
}

func TestCollectionHTTPWaitCapacityFallsBackToAcceptedOriginalReceipt(t *testing.T) {
	h := newCollectionHTTPFixture(t)
	scope := h.f.scope(t, h.project, h.worker, h.f.admin)
	receipt := h.f.enqueue(t, scope, "existing", Selection{[]int64{47}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	results := make(chan error, 8)
	for range 8 {
		go func() { _, err := h.e.Wait(ctx, scope, receipt.ID, 5*time.Second); results <- err }()
	}
	collectionWaitCount(t, h.e, 8)
	body, _ := collectionHTTPDo(t, h.request(t, "POST", h.path(), "capacity-accepted", `{"pull_numbers":[12]}`), 202)
	accepted := collectionHTTPReceipt(t, body)
	if accepted.ID == receipt.ID {
		t.Fatal("accepted fallback returned another action")
	}
	var original string
	if err := h.f.store.DB().QueryRow(`SELECT response_json FROM receipts WHERE operation='history.collect' AND key='capacity-accepted'`).Scan(&original); err != nil || strings.TrimSpace(string(body)) != original {
		t.Fatal("capacity fallback hid or changed the committed receipt", err)
	}
	before := collectionStoreRows(t, h.f)
	collectionHTTPDo(t, h.request(t, "GET", h.path()+"/"+receipt.ID, "", "", "wait=1"), 503)
	cancel()
	for range 8 {
		select {
		case err := <-results:
			collectionStoreCode(t, err, "overloaded")
		case <-time.After(time.Second):
			t.Fatal("owned capacity waiter failed to detach")
		}
	}
	collectionWaitCount(t, h.e, 0)
	if before != collectionStoreRows(t, h.f) || len(h.f.provider.calls) != 0 {
		t.Fatal("capacity waits changed actions or invoked provider work")
	}
}
