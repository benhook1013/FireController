package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/profile"
)

type identityBootstrapTransport func(*http.Request) (*http.Response, error)

func (f identityBootstrapTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func identityBootstrapFixture(t *testing.T, status int, media, body string) (*Client, *int) {
	t.Helper()
	base, _ := url.Parse("https://synthetic.invalid")
	calls := new(int)
	c := &Client{base: base, token: "synthetic-private-credential", identity: profile.Context{InstanceID: "instance-one"}}
	c.http = &http.Client{Transport: identityBootstrapTransport(func(r *http.Request) (*http.Response, error) {
		*calls++
		if r.Method != "GET" || r.URL.Path != "/api/v1/instance" || r.Header.Get("Authorization") != "Bearer synthetic-private-credential" || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("bootstrap retried another operation or binding")
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{media}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	return c, calls
}

func TestIdentityRetryAttemptAndDelayBoundsWithoutSleeping(t *testing.T) {
	c, calls := identityBootstrapFixture(t, 503, "text/plain", "synthetic unavailable")
	var instance struct {
		InstanceID string `json:"instance_id"`
	}
	var delays []time.Duration
	start := time.Now()
	err := c.verifyInstanceWithWait(t.Context(), &instance, func(ctx context.Context, delay time.Duration) error {
		deadline, ok := ctx.Deadline()
		if !ok || deadline.After(start.Add(2*requestTimeout+100*time.Millisecond)) {
			t.Fatal("bootstrap lost original time bound")
		}
		delays = append(delays, delay)
		return nil
	})
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second}
	var failure *Error
	if !errors.As(err, &failure) || failure.Status != 503 || failure.OutcomeUnknown || *calls != 7 || !reflect.DeepEqual(delays, want) {
		t.Fatal("retry count/backoff changed", err, *calls, delays)
	}
}

func TestIdentityRetryCancellationAndEnclosingDeadline(t *testing.T) {
	for _, cancelDuringWait := range []bool{true, false} {
		t.Run(map[bool]string{true: "cancel", false: "deadline"}[cancelDuringWait], func(t *testing.T) {
			c, calls := identityBootstrapFixture(t, 503, "text/plain", "synthetic unavailable")
			var ctx context.Context
			var cancel context.CancelFunc
			if cancelDuringWait {
				ctx, cancel = context.WithCancel(t.Context())
			} else {
				ctx, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
			}
			defer cancel()
			wait := waitIdentityRetry
			if cancelDuringWait {
				wait = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
			}
			start := time.Now()
			err := c.verifyInstanceWithWait(ctx, new(any), wait)
			var failure *Error
			want := "deadline_exceeded"
			if cancelDuringWait {
				want = "canceled"
			}
			if !errors.As(err, &failure) || failure.Code != want || failure.OutcomeUnknown || *calls != 1 || time.Since(start) > time.Second {
				t.Fatal("retry escaped cancellation/deadline", err, *calls)
			}
		})
	}
}

func TestIdentityRetryNeverReplaysPermanentOrMalformedReplies(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		media, body string
	}{
		{"malformed-json", 503, "application/json", `{"broken"`},
		{"malformed-success", 200, "application/json", `not json`},
		{"redirect", 302, "text/plain", "redirect"},
		{"unauthorized", 401, "text/plain", "denied"},
		{"forbidden", 403, "text/plain", "denied"},
		{"missing", 404, "text/plain", "missing"},
		{"mismatch-code", 503, "application/json", `{"error":{"code":"instance_mismatch","message":"synthetic mismatch"},"request_id":"request-one"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, calls := identityBootstrapFixture(t, tc.status, tc.media, tc.body)
			waits := 0
			err := c.verifyInstanceWithWait(t.Context(), new(any), func(context.Context, time.Duration) error { waits++; return nil })
			if err == nil || *calls != 1 || waits != 0 {
				t.Fatal("permanent or malformed reply retried", err, *calls, waits)
			}
		})
	}
}
