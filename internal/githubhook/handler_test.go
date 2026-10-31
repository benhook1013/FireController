package githubhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testSecret = []byte(strings.Repeat("s", 32))

const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testDelivery = "11111111-2222-4333-8444-000000000001"
const statusBody = `{"repository":{"id":42},"sha":"` + testSHA + `"}`

func signed(event, body, id string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/hook", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-GitHub-Event", event)
	r.Header.Set("X-GitHub-Delivery", id)
	mac := hmac.New(sha256.New, testSecret)
	_, _ = mac.Write([]byte(body))
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return r
}
func serve(h *Handler, event, body, id string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, signed(event, body, id))
	return w
}
func mustHandler(t *testing.T, cb Callback) *Handler {
	t.Helper()
	h, err := New(testSecret, cb)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestAuthenticationAndBounds(t *testing.T) {
	for _, secret := range [][]byte{nil, make([]byte, 31), make([]byte, 4097)} {
		if _, err := New(secret, func(context.Context, Hint) error { return nil }); err == nil {
			t.Fatal("accepted invalid secret length")
		}
	}
	if _, err := New(testSecret, nil); err == nil {
		t.Fatal("accepted absent callback")
	}
	var calls atomic.Int32
	secret := append([]byte(nil), testSecret...)
	h, err := New(secret, func(context.Context, Hint) error { calls.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	secret[0] = 'x'
	cases := []struct {
		name   string
		change func(*http.Request)
		status int
	}{
		{"copied secret", func(*http.Request) {}, 202},
		{"missing signature", func(r *http.Request) { r.Header.Del("X-Hub-Signature-256") }, 401},
		{"wrong secret", func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64)) }, 401},
		{"duplicate signature", func(r *http.Request) { r.Header.Add("X-Hub-Signature-256", r.Header.Get("X-Hub-Signature-256")) }, 401},
		{"bad signature encoding", func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("g", 64)) }, 401},
		{"bad delivery", func(r *http.Request) { r.Header.Set("X-GitHub-Delivery", "private-invalid") }, 400},
		{"duplicate delivery", func(r *http.Request) { r.Header.Add("X-GitHub-Delivery", testDelivery) }, 400},
		{"missing event", func(r *http.Request) { r.Header.Del("X-GitHub-Event") }, 400},
		{"wrong method", func(r *http.Request) { r.Method = "GET" }, 405},
		{"wrong media", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"compressed", func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }, 415},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := signed("status", statusBody, testDelivery)
			tc.change(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d", w.Code, tc.status)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing no-store")
			}
		})
	}
	if calls.Load() != 1 {
		t.Fatalf("callback count=%d", calls.Load())
	}
	r := signed("status", "private invalid JSON", testDelivery)
	r.Header.Set("X-Hub-Signature-256", "bad")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 || strings.Contains(w.Body.String(), "private") {
		t.Fatal("signature not rejected before decode or unsafe error")
	}
	if w := serve(h, "status", "private invalid JSON", testDelivery); w.Code != 400 {
		t.Fatalf("signed invalid JSON=%d", w.Code)
	}
	padded := strings.TrimSuffix(statusBody, "}") + `,"padding":"` + strings.Repeat("x", MaxBodyBytes-len(statusBody)-13) + `"}`
	if len(padded) != MaxBodyBytes {
		t.Fatalf("fixture size %d", len(padded))
	}
	if w := serve(h, "status", padded, "11111111-2222-4333-8444-000000000002"); w.Code != 202 {
		t.Fatalf("at bound=%d", w.Code)
	}
	if w := serve(h, "status", padded+" ", testDelivery); w.Code != 413 {
		t.Fatalf("above bound=%d", w.Code)
	}
}

func TestNormalizedEvents(t *testing.T) {
	pr := `{"action":"synchronize","repository":{"id":42},"number":7,"pull_request":{"number":7,"head":{"sha":"` + testSHA + `"},"base":{"ref":"feature/base"}}}`
	cases := []struct {
		name, event, body string
		hint              *Hint
	}{
		{"pull", "pull_request", pr, &Hint{RepositoryID: 42, Metadata: true, CI: true, PullNumbers: []int64{7}, HeadSHA: testSHA, BaseRef: "feature/base"}},
		{"status", "status", statusBody, &Hint{RepositoryID: 42, CI: true, HeadSHA: testSHA}},
		{"push", "push", `{"repository":{"id":42},"ref":"refs/heads/main","after":"` + testSHA + `","deleted":false}`, &Hint{RepositoryID: 42, Metadata: true, CI: true, HeadSHA: testSHA, BranchRef: "main"}},
		{"branch deletion", "push", `{"repository":{"id":42},"ref":"refs/heads/main","after":"` + strings.Repeat("0", 40) + `","deleted":true}`, &Hint{RepositoryID: 42, Metadata: true, CI: true, BranchRef: "main"}},
		{"tag", "push", `{"repository":{"id":42},"ref":"refs/tags/v1"}`, nil},
		{"ping", "ping", `{"zen":"synthetic"}`, nil},
		{"unsupported event", "issues", "ignored non JSON", nil},
		{"unsupported action", "pull_request", `{"action":"labeled"}`, nil},
	}
	for _, event := range []string{"check_run", "check_suite", "workflow_run"} {
		body := `{"repository":{"id":42},"action":"completed","` + event + `":{"head_sha":"` + testSHA + `","pull_requests":[{"number":9},{"number":7},{"number":9}]}}`
		cases = append(cases, struct {
			name, event, body string
			hint              *Hint
		}{event, event, body, &Hint{RepositoryID: 42, CI: true, PullNumbers: []int64{7, 9}, HeadSHA: testSHA}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []Hint
			h := mustHandler(t, func(_ context.Context, h Hint) error { got = append(got, h); return nil })
			w := serve(h, tc.event, tc.body, testDelivery)
			if tc.hint == nil {
				if w.Code != 204 || len(got) != 0 {
					t.Fatalf("ignored status=%d calls=%d", w.Code, len(got))
				}
				return
			}
			if w.Code != 202 || len(got) != 1 || !reflect.DeepEqual(got[0], *tc.hint) {
				t.Fatalf("status=%d hint=%+v want=%+v", w.Code, got, tc.hint)
			}
		})
	}
	// Every accepted lifecycle action is exercised independently rather than only
	// checking the parser's table. Unsupported actions never reach the callback.
	actions := map[string][]string{
		"pull_request": {"opened", "reopened", "closed", "synchronize", "edited", "ready_for_review", "converted_to_draft", "auto_merge_enabled", "auto_merge_disabled", "enqueued", "dequeued"},
		"check_run":    {"created", "completed", "rerequested", "requested_action"},
		"check_suite":  {"requested", "completed", "rerequested"},
		"workflow_run": {"requested", "in_progress", "completed"},
	}
	for event, list := range actions {
		for _, action := range list {
			t.Run(event+"/"+action, func(t *testing.T) {
				body := strings.Replace(pr, "synchronize", action, 1)
				if event != "pull_request" {
					body = `{"repository":{"id":42},"action":"` + action + `","` + event + `":{"head_sha":"` + testSHA + `"}}`
				}
				h := mustHandler(t, func(context.Context, Hint) error { return nil })
				if w := serve(h, event, body, testDelivery); w.Code != 202 {
					t.Fatalf("status=%d", w.Code)
				}
			})
		}
	}
}

func TestMalformedSelectors(t *testing.T) {
	cases := []struct{ name, event, body string }{
		{"null object", "status", `null`},
		{"missing repo", "status", `{"sha":"` + testSHA + `"}`},
		{"null repo", "status", `{"repository":null,"sha":"` + testSHA + `"}`},
		{"zero repo", "status", strings.Replace(statusBody, `42`, `0`, 1)},
		{"repo alias", "status", strings.Replace(statusBody, `"repository"`, `"Repository"`, 1)},
		{"duplicate repo", "status", strings.TrimSuffix(statusBody, "}") + `,"repository":{"id":42}}`},
		{"null sha", "status", `{"repository":{"id":42},"sha":null}`},
		{"uppercase sha", "status", strings.Replace(statusBody, testSHA, strings.ToUpper(testSHA), 1)},
		{"zero sha", "status", strings.Replace(statusBody, testSHA, strings.Repeat("0", 40), 1)},
		{"bad branch", "push", `{"repository":{"id":42},"ref":"refs/heads/a..b","after":"` + testSHA + `"}`},
		{"null delete", "push", `{"repository":{"id":42},"ref":"refs/heads/main","after":"` + testSHA + `","deleted":null}`},
		{"mismatched deletion", "push", `{"repository":{"id":42},"ref":"refs/heads/main","after":"` + testSHA + `","deleted":true}`},
	}
	for name, number := range map[string]string{"negative": "-1", "overflow": "2147483648", "null": "null", "mismatch": "8"} {
		cases = append(cases, struct{ name, event, body string }{name, "pull_request", `{"action":"opened","repository":{"id":42},"number":7,"pull_request":{"number":` + number + `,"head":{"sha":"` + testSHA + `"},"base":{"ref":"main"}}}`})
	}
	for name, list := range map[string]string{"null list": "null", "object list": "{}", "null item": "[null]", "zero number": "[{\"number\":0}]", "too many": "[" + strings.TrimSuffix(strings.Repeat(`{"number":1},`, 101), ",") + "]"} {
		cases = append(cases, struct{ name, event, body string }{name, "check_run", `{"action":"completed","repository":{"id":42},"check_run":{"head_sha":"` + testSHA + `","pull_requests":` + list + `}}`})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := mustHandler(t, func(context.Context, Hint) error { t.Fatal("invalid hint queued"); return nil })
			if w := serve(h, tc.event, tc.body, testDelivery); w.Code != 400 {
				t.Fatalf("status=%d", w.Code)
			}
		})
	}
}

func TestDeliveryDedupAndFailedRetry(t *testing.T) {
	calls := 0
	h := mustHandler(t, func(context.Context, Hint) error {
		calls++
		if calls == 1 {
			return errors.New("PRIVATE_CALLBACK_ERROR")
		}
		return nil
	})
	for _, want := range []int{503, 202, 202} {
		w := serve(h, "status", statusBody, testDelivery)
		if w.Code != want || strings.Contains(w.Body.String(), "PRIVATE") {
			t.Fatalf("status=%d want=%d", w.Code, want)
		}
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
	if w := serve(h, "status", strings.Replace(statusBody, "42", "43", 1), testDelivery); w.Code != 400 {
		t.Fatalf("conflict=%d", w.Code)
	}
	// A different event with identical raw bytes cannot reuse the same identity.
	h.deliveries[testDelivery].event = "push"
	if w := serve(h, "status", statusBody, testDelivery); w.Code != 400 {
		t.Fatalf("event conflict=%d", w.Code)
	}
}

func TestDeliveryConcurrency(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	h := mustHandler(t, func(context.Context, Hint) error {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return nil
	})
	done := make(chan int, 1)
	go func() { done <- serve(h, "status", statusBody, testDelivery).Code }()
	<-started
	if w := serve(h, "status", statusBody, testDelivery); w.Code != 503 {
		t.Fatalf("inflight duplicate=%d", w.Code)
	}
	// The callback is outside the cache lock: an unrelated delivery can queue.
	if w := serve(h, "status", statusBody, "11111111-2222-4333-8444-000000000002"); w.Code != 202 {
		t.Fatalf("other delivery=%d", w.Code)
	}
	close(release)
	if code := <-done; code != 202 {
		t.Fatalf("first=%d", code)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
	if w := serve(h, "status", statusBody, testDelivery); w.Code != 202 || calls.Load() != 2 {
		t.Fatal("completed delivery was requeued")
	}
}

func TestDeliveryCapacityTTLAndCancelledCallback(t *testing.T) {
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	calls := 0
	h := mustHandler(t, func(ctx context.Context, _ Hint) error { calls++; return ctx.Err() })
	h.now = func() time.Time { return now }
	for i := 0; i < deliveryLimit; i++ {
		h.deliveries[fmt.Sprint(i)] = &delivery{done: true, expires: now.Add(deliveryTTL)}
	}
	if w := serve(h, "status", statusBody, testDelivery); w.Code != 503 || calls != 0 {
		t.Fatal("capacity not bounded")
	}
	now = now.Add(deliveryTTL)
	if w := serve(h, "status", statusBody, testDelivery); w.Code != 202 || calls != 1 || len(h.deliveries) != 1 {
		t.Fatal("expired capacity not released")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := signed("status", statusBody, "11111111-2222-4333-8444-000000000002").WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 503 || len(h.deliveries) != 1 {
		t.Fatal("cancelled callback retained delivery claim")
	}
	if w := serve(h, "status", statusBody, "11111111-2222-4333-8444-000000000002"); w.Code != 202 {
		t.Fatal("cancelled delivery could not retry")
	}
}

// Verify CI list bounds before deduplication and the complete normalized shape
// remain independent of incidental GitHub facts the acquisition worker must read.
func TestCIHintWithoutPullNumbers(t *testing.T) {
	var got Hint
	h := mustHandler(t, func(_ context.Context, h Hint) error { got = h; return nil })
	body := map[string]any{"action": "completed", "repository": map[string]any{"id": 42}, "check_suite": map[string]any{"head_sha": testSHA, "conclusion": "success", "private": "PRIVATE_PAYLOAD"}}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := serve(h, "check_suite", string(raw), testDelivery)
	if w.Code != 202 || got.RepositoryID != 42 || !got.CI || got.Metadata || got.HeadSHA != testSHA || len(got.PullNumbers) != 0 || got.BaseRef != "" || got.BranchRef != "" || strings.Contains(w.Body.String(), "PRIVATE") {
		t.Fatalf("unexpected normalized hint %+v status=%d", got, w.Code)
	}
}
