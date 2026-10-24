package reviewprovider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func assertGitHubBodyReadFailure(t *testing.T, g *GitHub, ctx context.Context, method string) error {
	t.Helper()
	if method == http.MethodGet {
		data, headers, status, err := g.requestBytes(ctx, method, "/PRIVATE_PATH", nil, nil)
		if err == nil || data != nil || headers != nil || status != 0 {
			t.Fatal("failed GET exposed a partial response", status, err)
		}
		return err
	}
	dst := struct {
		Value string `json:"value"`
	}{Value: "unchanged"}
	err := g.requestJSON(ctx, method, "/PRIVATE_PATH", nil, []byte(`{"body":"PRIVATE_SUBMITTED_BODY"}`), http.StatusCreated, &dst)
	if err == nil || dst.Value != "unchanged" {
		t.Fatal("failed POST decoded partial response data", err)
	}
	return err
}

func assertGitHubBodyReadDiagnostic(t *testing.T, err error, code string, status int, unknown bool, privateURL string) {
	t.Helper()
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != code || failure.Status != status || failure.OutcomeUnknown != unknown || failure.RetryAfterSeconds != 0 {
		t.Fatalf("unexpected safe provider diagnostic: %+v", err)
	}
	for _, private := range []string{"PRIVATE_", privateURL} {
		if strings.Contains(fmt.Sprintf("%+v", err), private) || strings.Contains(fmt.Sprintf("%+v", failure), private) {
			t.Fatal("provider body read error disclosed transport or response data")
		}
	}
}

func TestGitHubBodyReadLoopbackFailuresRemainTransportFailures(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, mode := range []string{"client-timeout", "caller-deadline", "caller-cancellation", "truncated-body"} {
			t.Run(method+"/"+mode, func(t *testing.T) {
				var calls atomic.Int64
				entered, release := make(chan struct{}), make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Length", "1024")
					w.Header().Set("ETag", `"PRIVATE_ETAG"`)
					if method == http.MethodPost {
						w.WriteHeader(http.StatusCreated)
					}
					// Even a valid JSON prefix must be discarded when its promised
					// HTTP response body never completes successfully.
					_, _ = io.WriteString(w, `{"value":"PRIVATE_RESPONSE_BODY"}`)
					w.(http.Flusher).Flush()
					close(entered)
					if mode != "truncated-body" {
						select {
						case <-r.Context().Done():
						case <-release:
						}
					}
				}))
				defer server.Close()
				defer close(release)
				g := testGitHub(server, "PRIVATE_TOKEN")
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				switch mode {
				case "client-timeout":
					g.httpClient.Timeout = 150 * time.Millisecond
				case "caller-deadline":
					cancel()
					ctx, cancel = context.WithTimeout(t.Context(), 150*time.Millisecond)
					defer cancel()
				case "caller-cancellation":
					go func() {
						select {
						case <-entered:
							cancel()
						case <-release:
						}
					}()
				}
				err := assertGitHubBodyReadFailure(t, g, ctx, method)
				status := 0
				if mode == "client-timeout" || mode == "caller-deadline" {
					status = http.StatusRequestTimeout
				}
				assertGitHubBodyReadDiagnostic(t, err, "provider_unavailable", status, method == http.MethodPost, server.URL)
				if calls.Load() != 1 {
					t.Fatal("failed body read retried the request", calls.Load())
				}
				if len(g.cache) != 0 {
					t.Fatal("failed response body entered the conditional cache")
				}
				if mode == "caller-cancellation" && !errors.Is(err, context.Canceled) || mode == "caller-deadline" && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("caller cancellation identity was lost", err)
				}
				if mode == "client-timeout" && (ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded)) {
					t.Fatal("per-request timeout was mislabeled as caller deadline", err)
				}
			})
		}
	}
}

type githubBodyErrorReader struct{ err error }

func (r githubBodyErrorReader) Read([]byte) (int, error) { return 0, r.err }

type githubFaultResponseBody struct {
	io.Reader
	closed *atomic.Bool
}

func (b githubFaultResponseBody) Close() error { b.closed.Store(true); return nil }

func TestGitHubBodyReadSyntheticFailuresAreSafeAndCloseResponse(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, tc := range []struct {
			name   string
			err    error
			status int
		}{
			{"unexpected-eof", io.ErrUnexpectedEOF, 0},
			{"opaque-io", errors.New("PRIVATE_IO_ERROR /PRIVATE_PATH"), 0},
			{"wrapped-timeout", fmt.Errorf("PRIVATE_TIMEOUT at PRIVATE_URL: %w", os.ErrDeadlineExceeded), http.StatusRequestTimeout},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				var calls atomic.Int64
				var closed atomic.Bool
				g := &GitHub{token: "PRIVATE_TOKEN", baseURL: "http://127.0.0.1:1", httpClient: &http.Client{Transport: ciRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					status := http.StatusOK
					if method == http.MethodPost {
						status = http.StatusCreated
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Etag": {`"PRIVATE_ETAG"`}}, Body: githubFaultResponseBody{Reader: io.MultiReader(strings.NewReader(`{"value":"PRIVATE_RESPONSE_BODY"}`), githubBodyErrorReader{tc.err}), closed: &closed}, Request: r}, nil
				})}}
				err := assertGitHubBodyReadFailure(t, g, t.Context(), method)
				assertGitHubBodyReadDiagnostic(t, err, "provider_unavailable", tc.status, method == http.MethodPost, g.baseURL)
				if calls.Load() != 1 || !closed.Load() || len(g.cache) != 0 || errors.Is(err, tc.err) {
					t.Fatal("body fault retried, cached, leaked its cause or left response open", err)
				}
			})
		}
	}
}

func TestGitHubBodyReadTransportFailuresBeforeHeadersAndUnsentCancellation(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, mode := range []string{"opaque-transport", "transport-timeout", "unsent-cancellation", "unsent-deadline"} {
			t.Run(method+"/"+mode, func(t *testing.T) {
				var calls atomic.Int64
				raw := errors.New("PRIVATE_TRANSPORT_ERROR at PRIVATE_URL")
				if mode == "transport-timeout" {
					raw = fmt.Errorf("PRIVATE_TIMEOUT: %w", os.ErrDeadlineExceeded)
				}
				g := &GitHub{token: "PRIVATE_TOKEN", baseURL: "http://127.0.0.1:1", httpClient: &http.Client{Transport: ciRoundTripper(func(*http.Request) (*http.Response, error) {
					calls.Add(1)
					return nil, raw
				})}}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if mode == "unsent-cancellation" {
					cancel()
				} else if mode == "unsent-deadline" {
					cancel()
					ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
					defer cancel()
				}
				err := assertGitHubBodyReadFailure(t, g, ctx, method)
				status := 0
				if mode == "transport-timeout" || mode == "unsent-deadline" {
					status = http.StatusRequestTimeout
				}
				unsent := strings.HasPrefix(mode, "unsent-")
				assertGitHubBodyReadDiagnostic(t, err, "provider_unavailable", status, method == http.MethodPost && !unsent, g.baseURL)
				wantCalls := int64(1)
				if unsent {
					wantCalls = 0
				}
				if calls.Load() != wantCalls || errors.Is(err, raw) {
					t.Fatal("transport failure retried or retained a raw error", err)
				}
				if unsent && !errors.Is(err, ctx.Err()) || !unsent && errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("caller cancellation and request timeout identity were conflated", err)
				}
			})
		}
	}
}

func TestGitHubBodyReadCompleteMalformedAndOversizedResponsesStayInvalid(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, tc := range []struct{ name, body string }{
			{"malformed-json", `{"value":`},
			{"duplicate-json", `{"value":"PRIVATE_FIRST","value":"PRIVATE_SECOND"}`},
			{"invalid-utf8", "{\"value\":\"" + string([]byte{0xff}) + "\"}"},
			{"oversized", strings.Repeat(" ", githubMaxBody+1)},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				var calls atomic.Int64
				status := http.StatusOK
				if method == http.MethodPost {
					status = http.StatusCreated
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("ETag", `"PRIVATE_ETAG"`)
					w.WriteHeader(status)
					_, _ = io.WriteString(w, tc.body)
				}))
				defer server.Close()
				g := testGitHub(server, "PRIVATE_TOKEN")
				dst := struct {
					Value string `json:"value"`
				}{Value: "unchanged"}
				err := g.requestJSON(t.Context(), method, "/PRIVATE_PATH", nil, nil, status, &dst)
				assertGitHubBodyReadDiagnostic(t, err, "provider_invalid_response", status, method == http.MethodPost, server.URL)
				if calls.Load() != 1 || len(g.cache) != 0 || dst.Value != "unchanged" {
					t.Fatal("invalid completed response was retried, cached or partially decoded")
				}
			})
		}
	}
}

func TestGitHubBodyReadFailureCannotReplaceRetainedConditionalResponse(t *testing.T) {
	var calls atomic.Int64
	var closed atomic.Bool
	g := &GitHub{token: "PRIVATE_TOKEN", baseURL: "http://127.0.0.1:1", httpClient: &http.Client{Transport: ciRoundTripper(func(r *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		if call > 1 && r.Header.Get("If-None-Match") != `"old"` {
			t.Error("failed body changed the retained conditional identity")
		}
		response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Etag": {`"old"`}}, Body: io.NopCloser(strings.NewReader(`{"value":"old"}`)), Request: r}
		switch call {
		case 2:
			response.Header.Set("ETag", `"new"`)
			response.Body = githubFaultResponseBody{Reader: io.MultiReader(strings.NewReader(`{"value":"PRIVATE_NEW_BODY"}`), githubBodyErrorReader{io.ErrUnexpectedEOF}), closed: &closed}
		case 3:
			response.StatusCode = http.StatusNotModified
			response.Body = http.NoBody
		}
		return response, nil
	})}}
	var dst struct {
		Value string `json:"value"`
	}
	if err := g.getJSON(t.Context(), "/PRIVATE_PATH", nil, &dst); err != nil || dst.Value != "old" || calls.Load() != 1 {
		t.Fatal("initial conditional response failed", err)
	}
	err := g.getJSON(t.Context(), "/PRIVATE_PATH", nil, &dst)
	assertGitHubBodyReadDiagnostic(t, err, "provider_unavailable", 0, false, g.baseURL)
	if calls.Load() != 2 || !closed.Load() || dst.Value != "old" {
		t.Fatal("failed body retried or changed the caller's prior value")
	}
	if err := g.getJSON(t.Context(), "/PRIVATE_PATH", nil, &dst); err != nil || calls.Load() != 3 || dst.Value != "old" {
		t.Fatal("explicit revalidation adopted failed body bytes", err)
	}
}
