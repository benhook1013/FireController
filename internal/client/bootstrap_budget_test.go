package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/benhook1013/FireController/internal/profile"
)

func TestIdentityBootstrapSlowTLSReusesConnectionAndRevalidatesIdentity(t *testing.T) {
	const token = "synthetic-bootstrap-credential"
	request := SendMessageRequest{RecipientWorkerID: "worker-two", Body: "Preserve this exact message.\n"}
	wantBody, _ := json.Marshal(request)
	var connections, handshakes, identities, mutations atomic.Int64
	var replacement atomic.Bool
	service := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-FC-Instance") != "instance-one" {
			t.Error("bootstrap changed credential or instance pin")
		}
		if r.URL.Path == "/api/v1/instance" {
			identities.Add(1)
			if r.Method != http.MethodGet || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("Prefer") != "" {
				t.Error("identity request changed its read-only contract")
			}
			instance := "instance-one"
			if replacement.Load() {
				instance = "instance-replacement"
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": instance})
			return
		}
		mutations.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil || r.Method != http.MethodPost || r.URL.Path != "/api/v1/projects/project-one/messages" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Idempotency-Key") != "original-bootstrap-key" || string(body) != string(wantBody) {
			t.Error("selected write changed body, key or binding")
		}
		// Simulate a gateway response after the effect. The client cannot know
		// whether the write committed and must never repeat it automatically.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"code":"overloaded","message":"synthetic lost receipt"},"request_id":"request-one"}`)
	}))
	service.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	service.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if handshakes.Add(1) == 1 {
			timer := time.NewTimer(requestTimeout + 200*time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-hello.Context().Done():
				return nil, hello.Context().Err()
			}
		}
		return nil, nil
	}}
	service.StartTLS()
	defer service.Close()
	transport := newTransport()
	transport.TLSClientConfig = service.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	pool := &TransportPool{transport: transport, sessionID: "synthetic-bootstrap-session"}
	defer pool.Close()
	credential := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credential, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	binding := profile.Binding{Context: profile.Context{Profile: "selected", ServerURL: service.URL, InstanceID: "instance-one", ProjectID: "project-one", WorkerID: "worker-one"}, CredentialFile: credential}
	c, err := NewWithTransportPool(t.Context(), binding, pool)
	if err != nil {
		t.Fatal("slow first TLS connection failed inside bootstrap budget", err)
	}
	defer c.Close()
	if connections.Load() != 1 || handshakes.Load() != 1 || identities.Load() != 1 || mutations.Load() != 0 {
		t.Fatal("bootstrap restarted the slow connection or sent work", connections.Load(), handshakes.Load(), identities.Load(), mutations.Load())
	}
	_, err = c.SendMessage(t.Context(), "original-bootstrap-key", request)
	var failure *Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown || mutations.Load() != 1 || connections.Load() != 1 {
		t.Fatal("unknown write replayed or bootstrap discarded the connection", err, mutations.Load(), connections.Load())
	}
	c.Close()
	replacement.Store(true)
	other, err := NewWithTransportPool(t.Context(), binding, pool)
	if other != nil || !errors.As(err, &failure) || failure.Code != "instance_mismatch" || failure.OutcomeUnknown || identities.Load() != 2 || connections.Load() != 1 || mutations.Load() != 1 {
		t.Fatal("pooled connection bypassed identity revalidation", err, identities.Load(), connections.Load(), mutations.Load())
	}
}

func TestIdentityBootstrapBudgetDoesNotExtendOperationOrReplayMutation(t *testing.T) {
	for _, mutation := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "mutation"}[mutation], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				base, _ := url.Parse("https://synthetic.invalid")
				calls := 0
				transport := identityBootstrapTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						select {
						case <-time.After(6 * time.Second):
							return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"instance_id":"instance-one"}`))}, nil
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
					}
					if r.Method == http.MethodPost && (r.GetBody != nil || r.Header.Get("Idempotency-Key") != "original-key") {
						t.Error("write became replayable or changed its key")
					}
					<-r.Context().Done()
					return nil, r.Context().Err()
				})
				c := &Client{base: base, token: "synthetic-private-credential", credentialFile: "synthetic-credential-path", identity: profile.Context{InstanceID: "instance-one"}, http: &http.Client{Transport: transport, Timeout: requestTimeout}}
				start := time.Now()
				var instance struct {
					InstanceID string `json:"instance_id"`
				}
				if err := c.verifyInstance(t.Context(), &instance); err != nil || calls != 1 || time.Since(start) != 6*time.Second {
					t.Fatal("identity GET used an operation deadline", err, calls, time.Since(start))
				}
				start = time.Now()
				var err error
				if mutation {
					err = c.mutate(t.Context(), http.MethodPost, []string{"api", "v1", "projects", "project-one", "jobs"}, "original-key", map[string]string{"title": "Original"}, 201, new(any))
				} else {
					err = c.get(t.Context(), []string{"api", "v1", "projects", "project-one", "workers"}, nil, true, new(any))
				}
				var failure *Error
				if !errors.As(err, &failure) || failure.Code != "deadline_exceeded" || failure.OutcomeUnknown != mutation || calls != 2 || time.Since(start) != requestTimeout || c.http.Timeout != requestTimeout {
					t.Fatal("operation lost its deadline, uncertainty or single-shot boundary", err, calls, time.Since(start), c.http.Timeout)
				}
			})
		})
	}
}

func TestIdentityBootstrapRetriesShareDeadlineAndRespectCaller(t *testing.T) {
	for _, mode := range []string{"bootstrap", "caller-deadline", "caller-cancel"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, calls := identityBootstrapFixture(t, 503, "text/plain", "synthetic unavailable")
				first := c.http.Transport
				c.http.Transport = identityBootstrapTransport(func(r *http.Request) (*http.Response, error) {
					if *calls == 0 {
						select {
						case <-time.After(4 * time.Second):
							return first.RoundTrip(r)
						case <-r.Context().Done():
							*calls++
							return nil, r.Context().Err()
						}
					}
					*calls++
					<-r.Context().Done()
					return nil, r.Context().Err()
				})
				ctx := t.Context()
				wantDuration, wantCalls, wantCode := bootstrapTimeout, 2, "deadline_exceeded"
				if mode == "caller-deadline" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
					defer cancel()
					wantDuration, wantCalls = 2*time.Second, 1
				} else if mode == "caller-cancel" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					defer cancel()
					time.AfterFunc(time.Second, cancel)
					wantDuration, wantCalls, wantCode = time.Second, 1, "canceled"
				}
				start := time.Now()
				err := c.verifyInstance(ctx, new(any))
				var failure *Error
				if !errors.As(err, &failure) || failure.Code != wantCode || failure.OutcomeUnknown || *calls != wantCalls || time.Since(start) != wantDuration {
					t.Fatal("identity attempts extended deadline or escaped cancellation", err, *calls, time.Since(start))
				}
			})
		})
	}
}

func TestIdentityConnectionFailureRetainsConfigurationGuidance(t *testing.T) {
	credential := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credential, []byte("synthetic-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		transport := newTransport()
		var calls atomic.Int64
		transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
			calls.Add(1)
			return nil, errors.New("PRIVATE_DIAL_DETAIL")
		}
		pool := &TransportPool{transport: transport}
		defer pool.Close()
		b := profile.Binding{Context: profile.Context{Profile: "selected", ServerURL: "http://127.0.0.1:1", InstanceID: "instance-one", ProjectID: "project-one", WorkerID: "worker-one"}, CredentialFile: credential}
		c, err := NewWithTransportPool(t.Context(), b, pool)
		var failure *Error
		if c != nil || !errors.As(err, &failure) || failure.Code != "connection_failed" || failure.OutcomeUnknown || calls.Load() != 7 {
			t.Fatal("connection retry contract changed", c, err, calls.Load())
		}
		if strings.Contains(failure.Message, "temporarily unavailable") || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("connection error overstated transience or exposed details", err)
		}
		for _, phrase := range []string{"service address", "TLS", "transport", "idempotency key", "earlier uncertain write"} {
			if !strings.Contains(failure.Action, phrase) {
				t.Fatal("missing configuration/recovery guidance", phrase, failure.Action)
			}
		}
	})
}
