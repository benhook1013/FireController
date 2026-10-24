package historycollect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

type collectionHTTPFixture struct {
	f               *collectionStoreFixture
	e               *Engine
	project, worker string
	server          *httptest.Server
}

func newCollectionHTTPFixture(t *testing.T, writeBudget ...time.Duration) *collectionHTTPFixture {
	t.Helper()
	f := newCollectionStoreFixture(t)
	p, w := f.project(t, "selected/repository")
	e := f.engine(t)
	e.Register(f.handler.(*http.ServeMux))
	h := &collectionHTTPFixture{f: f, e: e, project: p, worker: w}
	h.server = httptest.NewUnstartedServer(f.handler)
	h.server.Config.WriteTimeout = 5 * time.Second
	if len(writeBudget) != 0 {
		h.server.Config.WriteTimeout = writeBudget[0]
	}
	h.server.Start()
	t.Cleanup(h.server.Close)
	return h
}
func (h *collectionHTTPFixture) path() string {
	return "/api/v1/projects/" + h.project + "/history/collections"
}
func (h *collectionHTTPFixture) request(t *testing.T, method, path, key, body string, prefer ...string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, h.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+h.f.admin)
	r.Header.Set("X-FC-Instance", h.f.store.InstanceID())
	r.Header.Set("X-FC-Worker", h.worker)
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	for _, value := range prefer {
		r.Header.Add("Prefer", value)
	}
	return r
}
func collectionHTTPDo(t *testing.T, r *http.Request, status int) ([]byte, http.Header) {
	t.Helper()
	client := &http.Client{Timeout: 4 * time.Second}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal("collection HTTP transport failed", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(body) > 64<<10 || response.StatusCode != status {
		t.Fatalf("collection HTTP status=%d want=%d, read error=%v", response.StatusCode, status, err)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("collection response missing private cache policy")
	}
	return body, response.Header
}
func collectionHTTPReceipt(t *testing.T, body []byte) Receipt {
	t.Helper()
	var receipt Receipt
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &receipt) != nil || json.Unmarshal(body, &fields) != nil || len(fields) != 3 || !operationPattern.MatchString(receipt.ID) || receipt.State != "queued" || receipt.Revision != 1 || len(body) > 256 {
		t.Fatal("accepted response is not the original compact queued receipt")
	}
	return receipt
}

func TestCollectionHTTPDefaultWaitReturnsCommittedUsefulResult(t *testing.T) {
	for _, paused := range []bool{false, true} {
		name := "completed"
		if paused {
			name = "paused"
		}
		t.Run(name, func(t *testing.T) {
			h := newCollectionHTTPFixture(t)
			if paused {
				h.f.provider.repository.ID++
			}
			ctx, cancel := context.WithCancel(t.Context())
			done, err := h.e.Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { cancel(); collectionJoin(t, done) }()
			body, headers := collectionHTTPDo(t, h.request(t, "POST", h.path(), "default-wait", `{"pull_numbers":[12]}`), 200)
			var operation Operation
			if json.Unmarshal(body, &operation) != nil || operation.State != name || headers.Get("Preference-Applied") != "wait=2" || !strings.Contains(headers.Get("Vary"), "Prefer") {
				t.Fatal("default wait did not return a useful terminal observation")
			}
			scope := h.f.scope(t, h.project, h.worker, h.f.admin)
			stored := collectionStoreRead(t, h.e, scope, operation.ID)
			if stored.Revision != operation.Revision || stored.PageCount != operation.PageCount || stored.State != operation.State {
				t.Fatal("terminal HTTP result preceded its durable commit")
			}
		})
	}
}

func TestCollectionHTTPReceiptReplayIsIndependentOfWaitAndLostResponse(t *testing.T) {
	h := newCollectionHTTPFixture(t)
	var dropped atomic.Bool
	var admissions atomic.Int64
	drop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == h.path() {
			admissions.Add(1)
			if !dropped.Swap(true) {
				recorded := httptest.NewRecorder()
				h.f.handler.ServeHTTP(recorded, r)
				if recorded.Code != 202 {
					t.Error("lost-response fixture did not actually accept the action")
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
				return
			}
		}
		h.f.handler.ServeHTTP(w, r)
	}))
	defer drop.Close()
	r := h.request(t, "POST", h.path(), "recover-original", `{"pull_numbers":[47]}`, "wait=0")
	r.URL.Scheme, r.URL.Host = "http", strings.TrimPrefix(drop.URL, "http://")
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	response, err := client.Do(r)
	if response != nil {
		response.Body.Close()
	}
	if err == nil || admissions.Load() != 1 {
		t.Fatal("response loss was not a single accepted request")
	}
	var original string
	if err := h.f.store.DB().QueryRow(`SELECT response_json FROM receipts WHERE operation='history.collect' AND key='recover-original'`).Scan(&original); err != nil {
		t.Fatal("accepted lost response was not committed", err)
	}
	body, headers := collectionHTTPDo(t, h.request(t, "POST", h.path(), "recover-original", `{"pull_numbers":[47]}`, "wait=0"), 202)
	receipt := collectionHTTPReceipt(t, body)
	if strings.TrimSpace(string(body)) != original || headers.Get("Preference-Applied") != "wait=0" {
		t.Fatal("exact retry did not recover the original receipt")
	}
	canceled, _ := collectionHTTPDo(t, h.request(t, "POST", h.path()+"/"+receipt.ID+"/cancel", "explicit-cancel", `{"expected_revision":1}`), 200)
	var cancelReceipt Receipt
	if json.Unmarshal(canceled, &cancelReceipt) != nil || cancelReceipt.ID != receipt.ID || cancelReceipt.State != "canceled" || cancelReceipt.Revision != 2 {
		t.Fatal("explicit HTTP cancellation did not return its committed receipt")
	}
	before := collectionStoreRows(t, h.f)
	body, headers = collectionHTTPDo(t, h.request(t, "POST", h.path(), "recover-original", `{"pull_numbers":[47]}`, "wait=900"), 200)
	var operation Operation
	if json.Unmarshal(body, &operation) != nil || operation.ID != receipt.ID || operation.State != "canceled" || headers.Get("Preference-Applied") != "wait=900" {
		t.Fatal("wait preference changed action identity")
	}
	body, _ = collectionHTTPDo(t, h.request(t, "POST", h.path(), "recover-original", `{"pull_numbers":[47]}`, "wait=0"), 202)
	if strings.TrimSpace(string(body)) != original || before != collectionStoreRows(t, h.f) || len(h.f.provider.calls) != 0 {
		t.Fatal("replay rewrote the original receipt or invoked provider work")
	}
}

func TestCollectionHTTPAdmissionErrorsAreProviderFreeAndAtomic(t *testing.T) {
	h := newCollectionHTTPFixture(t)
	for _, tc := range []struct {
		name, suffix, key, body string
		prefer                  []string
	}{
		{"duplicate-prefer", "", "key", `{"pull_numbers":[47]}`, []string{"wait=0", "wait=1"}},
		{"malformed-prefer", "", "key", `{"pull_numbers":[47]}`, []string{"wait=01"}},
		{"query", "?wait=0", "key", `{"pull_numbers":[47]}`, []string{"wait=0"}},
		{"duplicate-json", "", "key", `{"pull_numbers":[47],"pull_numbers":[12]}`, []string{"wait=0"}},
		{"unknown-input", "", "key", `{"pull_numbers":[47],"apply":true}`, []string{"wait=0"}},
		{"duplicate-selection", "", "key", `{"pull_numbers":[47,47]}`, []string{"wait=0"}},
		{"missing-key", "", "", `{"pull_numbers":[47]}`, []string{"wait=0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := collectionStoreRows(t, h.f)
			collectionHTTPDo(t, h.request(t, "POST", h.path()+tc.suffix, tc.key, tc.body, tc.prefer...), 400)
			if before != collectionStoreRows(t, h.f) || len(h.f.provider.calls) != 0 {
				t.Fatal("invalid request changed admission/provider state")
			}
		})
	}
}

func TestCollectionHTTPCurrentAuthorityCookieAndProjectIsolation(t *testing.T) {
	h := newCollectionHTTPFixture(t)
	body, _ := collectionHTTPDo(t, h.request(t, "POST", h.path(), "one", `{"pull_numbers":[47]}`, "wait=0"), 202)
	id := collectionHTTPReceipt(t, body).ID
	other, otherWorker := h.f.project(t, "other/repository")
	foreign := h.request(t, "GET", "/api/v1/projects/"+other+"/history/collections/"+id, "", "")
	foreign.Header.Set("X-FC-Worker", otherWorker)
	before := collectionStoreRows(t, h.f)
	collectionHTTPDo(t, foreign, 404)
	if before != collectionStoreRows(t, h.f) {
		t.Fatal("foreign-project read changed retained state")
	}
	for _, permissions := range []string{`["read"]`, `["read","write"]`} {
		token := core.NewID("ordinary")
		sum := sha256.Sum256([]byte(token))
		h.f.request(t, "POST", "/api/v1/projects/"+h.project+"/credentials", h.f.admin, "", `{"token_sha256":"`+hex.EncodeToString(sum[:])+`","permissions":`+permissions+`}`, 200)
		before = collectionStoreRows(t, h.f)
		for _, method := range []string{"GET", "POST"} {
			path := h.path()
			if method == "GET" {
				path += "/" + id
			}
			r := h.request(t, method, path, "ordinary", `{"pull_numbers":[47]}`, "wait=0")
			r.Header.Set("Authorization", "Bearer "+token)
			collectionHTTPDo(t, r, 403)
		}
		if before != collectionStoreRows(t, h.f) {
			t.Fatal("ordinary credential refusal changed retained state")
		}
	}
	session := strings.Repeat("b", 32)
	cookie := h.f.request(t, "POST", "/api/v1/projects/"+h.project+"/browser-session", h.f.admin, "", `{"session_id":"`+session+`"}`, 200)
	before = collectionStoreRows(t, h.f)
	r := h.request(t, "GET", h.path()+"/"+id, "", "")
	r.Header.Del("Authorization")
	r.Header.Set("X-FC-Browser-Session", session)
	for _, c := range cookie.Result().Cookies() {
		r.AddCookie(c)
	}
	collectionHTTPDo(t, r, 403)
	if before != collectionStoreRows(t, h.f) || len(h.f.provider.calls) != 0 {
		t.Fatal("rejected authority altered acquisition rows or invoked providers")
	}
}

func TestCollectionHTTPLifetimePOSTPreservesReceiptOnlyWithCurrentAuthority(t *testing.T) {
	for _, kind := range []string{"authorized", "revoked", "read-failed"} {
		t.Run(kind, func(t *testing.T) {
			h := newCollectionHTTPFixture(t)
			lifetime, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.e.waitLifetime = lifetime
			request := h.request(t, "POST", h.path(), "shutdown-receipt", `{"pull_numbers":[47]}`, "wait=900")
			type result struct {
				response *http.Response
				err      error
			}
			done := make(chan result, 1)
			go func() {
				response, err := (&http.Client{Timeout: 4 * time.Second}).Do(request)
				done <- result{response, err}
			}()
			collectionWaitCount(t, h.e, 1)
			var original string
			if err := h.f.store.DB().QueryRow(`SELECT response_json FROM receipts WHERE operation='history.collect' AND key='shutdown-receipt'`).Scan(&original); err != nil {
				t.Fatal(err)
			}
			before := collectionStoreRows(t, h.f)
			switch kind {
			case "revoked":
				if _, err := h.f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE is_admin=1`); err != nil {
					t.Fatal(err)
				}
			case "read-failed":
				if err := h.f.store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			cancel()
			select {
			case got := <-done:
				if got.err != nil {
					t.Fatal("lifetime POST transport failed", got.err)
				}
				defer got.response.Body.Close()
				data, err := io.ReadAll(io.LimitReader(got.response.Body, 64<<10))
				if err != nil {
					t.Fatal(err)
				}
				if kind == "authorized" {
					receipt := collectionHTTPReceipt(t, data)
					if got.response.StatusCode != 202 || strings.TrimSpace(string(data)) != original || receipt.State != "queued" || strings.Contains(string(data), "outcome_unknown") {
						t.Fatal("shutdown did not return exact known accepted receipt")
					}
					// Losing that response is recoverable under the same key even while the
					// observer lifetime is ended. No provider work or new receipt is needed.
					replay, _ := collectionHTTPDo(t, h.request(t, "POST", h.path(), "shutdown-receipt", `{"pull_numbers":[47]}`, "wait=0"), 202)
					if strings.TrimSpace(string(replay)) != original {
						t.Fatal("lost-output retry changed original receipt")
					}
				} else {
					want := 401
					if kind == "read-failed" {
						want = 500
					}
					if got.response.StatusCode != want || !strings.Contains(string(data), `"outcome_unknown":true`) {
						t.Fatal("shutdown masked failed current authorization/read", got.response.StatusCode)
					}
				}
			case <-time.After(time.Second):
				t.Fatal("lifetime failed to detach committed HTTP POST")
			}
			collectionWaitCount(t, h.e, 0)
			if kind == "read-failed" {
				h.f.open(t)
			}
			if before != collectionStoreRows(t, h.f) || len(h.f.provider.calls) != 0 {
				t.Fatal("observer shutdown changed accepted work, captures, receipts or events")
			}
		})
	}
}
