package githubhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func telemetryRows(h *Handler) map[hookOutcomeKey]uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[hookOutcomeKey]uint64)
	for k, v := range h.telemetry.rows {
		out[k] = v
	}
	return out
}

func TestTelemetrySignedHandlerOutcomes(t *testing.T) {
	h := mustHandler(t, func(context.Context, Hint) error { return nil })
	for _, tc := range []struct {
		event, body, id string
		code            int
	}{
		{"status", statusBody, testDelivery, 202},
		{"status", statusBody, testDelivery, 202},
		{"status", statusBody + " ", testDelivery, 400},
		{"ping", `{"zen":"private-payload"}`, testDelivery, 204},
		{"private-event", "private-payload", testDelivery, 204},
		{"status", "private-invalid-json", testDelivery, 400},
	} {
		if got := serve(h, tc.event, tc.body, tc.id).Code; got != tc.code {
			t.Fatalf("status=%d want=%d", got, tc.code)
		}
	}
	r := signed("status", statusBody, testDelivery)
	r.Header.Set("X-Hub-Signature-256", "invalid-private-signature")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	r = signed("status", statusBody, testDelivery)
	r.Method = "GET"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
	want := map[hookOutcomeKey]uint64{
		{"status", "admitted"}: 1, {"status", "duplicate"}: 1, {"status", "conflict"}: 1,
		{"ping", "ignored"}: 1, {"other", "ignored"}: 1, {"status", "rejected_request"}: 1,
		{"unverified", "rejected_signature"}: 1, {"unverified", "rejected_request"}: 1,
	}
	if got := telemetryRows(h); !reflect.DeepEqual(got, want) {
		t.Fatalf("outcomes=%v want=%v", got, want)
	}
}

func TestTelemetryFailureCapacityAndInflight(t *testing.T) {
	t.Run("callback failure retries", func(t *testing.T) {
		var calls int
		h := mustHandler(t, func(context.Context, Hint) error {
			calls++
			if calls == 1 {
				return errors.New("private callback error")
			}
			return nil
		})
		if serve(h, "status", statusBody, testDelivery).Code != 503 || serve(h, "status", statusBody, testDelivery).Code != 202 {
			t.Fatal("retry behavior changed")
		}
		want := map[hookOutcomeKey]uint64{{"status", "callback_failed"}: 1, {"status", "admitted"}: 1}
		if got := telemetryRows(h); !reflect.DeepEqual(got, want) {
			t.Fatal(got)
		}
	})
	t.Run("capacity", func(t *testing.T) {
		h := mustHandler(t, func(context.Context, Hint) error { t.Error("unexpected callback"); return nil })
		for i := 0; i < deliveryLimit; i++ {
			h.deliveries[fmt.Sprint(i)] = &delivery{}
		}
		if serve(h, "status", statusBody, testDelivery).Code != 503 {
			t.Fatal("capacity status")
		}
		if got := telemetryRows(h); !reflect.DeepEqual(got, map[hookOutcomeKey]uint64{{"status", "capacity"}: 1}) {
			t.Fatal(got)
		}
	})
	t.Run("inflight", func(t *testing.T) {
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan int, 1)
		var once sync.Once
		defer once.Do(func() { close(release) })
		h := mustHandler(t, func(context.Context, Hint) error { close(entered); <-release; return nil })
		go func() { done <- serve(h, "status", statusBody, testDelivery).Code }()
		<-entered
		if serve(h, "status", statusBody, testDelivery).Code != 503 {
			t.Error("inflight status")
		}
		once.Do(func() { close(release) })
		if <-done != 202 {
			t.Error("initial status")
		}
		if got := telemetryRows(h); !reflect.DeepEqual(got, map[hookOutcomeKey]uint64{{"status", "inflight"}: 1, {"status", "admitted"}: 1}) {
			t.Fatal(got)
		}
	})
}

func TestTelemetryConcurrentRolloverAndSanitization(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	start := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	var seconds atomic.Int64
	var callbacks atomic.Int64
	h := mustHandler(t, func(context.Context, Hint) error { callbacks.Add(1); return nil })
	h.now = func() time.Time { return start.Add(time.Duration(seconds.Load()) * time.Second) }
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("11111111-2222-4333-8444-%012d", i+1)
			if serve(h, "status", statusBody, id).Code != 202 {
				t.Error("delivery refused")
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < 100; i++ {
		h.observeOutcome(t.Context(), fmt.Sprintf("private-event-%d", i), fmt.Sprintf("private-outcome-%d", i))
	}
	if callbacks.Load() != 64 {
		t.Fatal(callbacks.Load())
	}
	want := map[hookOutcomeKey]uint64{{"status", "admitted"}: 64, {"unverified", "rejected_request"}: 100}
	if got := telemetryRows(h); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if logs.Len() != 0 {
		t.Fatal("logged before event-driven rollover")
	}
	seconds.Store(60)
	// Concurrent boundary deliveries must emit each old aggregate only once.
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); serve(h, "ping", `{"zen":"private-payload"}`, testDelivery) }()
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("aggregate lines=%d: %s", len(lines), logs.String())
	}
	seen := map[hookOutcomeKey]uint64{}
	for _, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["msg"] != "GitHub webhook delivery totals" || row["window_ended"] != start.Add(time.Minute).Format(time.RFC3339) {
			t.Fatal(row)
		}
		for key := range row {
			switch key {
			case "time", "level", "msg", "window_ended", "event", "outcome", "deliveries":
			default:
				t.Fatalf("unexpected field %s", key)
			}
		}
		seen[hookOutcomeKey{row["event"].(string), row["outcome"].(string)}] = uint64(row["deliveries"].(float64))
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("logs=%v want=%v", seen, want)
	}
	for _, private := range []string{testDelivery, testSHA, string(testSecret), "private-", "repository", "payload", "signature"} {
		if strings.Contains(logs.String(), private) {
			t.Fatalf("private material logged: %s", private)
		}
	}
	if got := telemetryRows(h); !reflect.DeepEqual(got, map[hookOutcomeKey]uint64{{"ping", "ignored"}: 16}) {
		t.Fatal(got)
	}
	// A quiet gap creates no empty intermediate windows.
	seconds.Store(3600)
	h.observeOutcome(t.Context(), "other", "ignored")
	if len(strings.Split(strings.TrimSpace(logs.String()), "\n")) != 3 {
		t.Fatal("unexpected quiet-window emission")
	}
}
