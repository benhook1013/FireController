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
	"testing/synctest"
	"time"

	"github.com/benhook1013/FireController/internal/profile"
)

type identityBootstrapTransport func(*http.Request) (*http.Response, error)

func (f identityBootstrapTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func identityBootstrapFixture(t *testing.T, status int, media, body string) (*Client, *int) {
	t.Helper()
	base, _ := url.Parse("https://synthetic.invalid")
	calls := new(int)
	c := &Client{base: base, token: "synthetic-private-credential", credentialFile: "/synthetic/credential", identity: profile.Context{InstanceID: "instance-one"}}
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

// Close runs after the response has been classified but before the retry loop
// examines it, so cancellation never depends on HTTP or scheduler timing.
type identityBootstrapCloseBody struct {
	io.ReadCloser
	onClose func()
}

func (b identityBootstrapCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.onClose()
	return err
}

func identityBootstrapOnClose(c *Client, onClose func()) {
	transport := c.http.Transport
	c.http.Transport = identityBootstrapTransport(func(r *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(r)
		if err == nil {
			response.Body = identityBootstrapCloseBody{ReadCloser: response.Body, onClose: onClose}
		}
		return response, err
	})
}

func TestIdentityRetryCancellationAfterResponse(t *testing.T) {
	for _, code := range []string{"canceled", "deadline_exceeded"} {
		for _, attempt := range []int{1, 7} {
			name := code + "/first_attempt"
			if attempt == 7 {
				name = code + "/last_attempt"
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					c, calls := identityBootstrapFixture(t, 503, "text/plain", "synthetic unavailable")
					var ctx context.Context
					var cancel context.CancelFunc
					if code == "canceled" {
						ctx, cancel = context.WithCancel(t.Context())
					} else {
						ctx, cancel = context.WithTimeout(t.Context(), time.Second)
					}
					defer cancel()
					identityBootstrapOnClose(c, func() {
						if *calls == attempt {
							if code == "canceled" {
								cancel()
							}
							<-ctx.Done()
						}
					})
					waits := 0
					err := c.verifyInstanceWithWait(ctx, new(any), func(context.Context, time.Duration) error {
						waits++
						return nil
					})
					var failure *Error
					if !errors.As(err, &failure) || failure.Code != code || failure.Status != 0 || failure.OutcomeUnknown || *calls != attempt || waits != attempt-1 {
						t.Fatalf("response hid %s: error=%#v calls=%d waits=%d", code, err, *calls, waits)
					}
				})
			})
		}
	}
}

func TestIdentityRetryPreservesTerminalResponseAfterCancellation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		media, body string
		code        string
	}{
		{"success", 200, "application/json", `{"instance_id":"instance-one"}`, ""},
		{"malformed-json", 503, "application/json", `{"broken"`, "invalid_response"},
		{"unauthorized", 401, "application/json", `{"error":{"code":"unauthorized","message":"synthetic refusal"},"request_id":"request-one"}`, "unauthorized"},
		{"mismatch", 503, "application/json", `{"error":{"code":"instance_mismatch","message":"synthetic mismatch"},"request_id":"request-one"}`, "instance_mismatch"},
		{"redirect", 302, "text/plain", "redirect", "redirect_rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, calls := identityBootstrapFixture(t, tc.status, tc.media, tc.body)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			identityBootstrapOnClose(c, cancel)
			waits := 0
			err := c.verifyInstanceWithWait(ctx, new(any), func(context.Context, time.Duration) error { waits++; return nil })
			var failure *Error
			if tc.code == "" && err != nil || tc.code != "" && (!errors.As(err, &failure) || failure.Code != tc.code || failure.OutcomeUnknown) {
				t.Fatalf("cancellation replaced a terminal response: %#v", err)
			}
			if *calls != 1 || waits != 0 {
				t.Fatal("terminal response retried", *calls, waits)
			}
		})
	}
}
