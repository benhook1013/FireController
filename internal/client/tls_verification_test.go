package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/profile"
)

func TestIdentityBootstrapRejectsUntrustedTLSWithoutRetry(t *testing.T) {
	var connections, requests atomic.Int64
	service := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	service.Config.ErrorLog = log.New(io.Discard, "", 0)
	service.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	service.StartTLS()
	defer service.Close()
	credential := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credential, []byte("synthetic-tls-test-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	binding := profile.Binding{Context: profile.Context{Profile: "selected", ServerURL: service.URL, InstanceID: "instance-one", ProjectID: "project-one", WorkerID: "worker-one"}, CredentialFile: credential}
	// Use the ordinary trust configuration: this server's certificate is untrusted.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	c, err := New(ctx, binding)
	var failure *Error
	if c != nil || !errors.As(err, &failure) || failure.Code != "tls_verification_failed" || failure.OutcomeUnknown || connections.Load() != 1 || requests.Load() != 0 {
		t.Fatal("certificate failure retried or dispatched work", c, err, connections.Load(), requests.Load())
	}
	if !strings.Contains(failure.Action, "Check the configured service address, TLS and transport") || !strings.Contains(failure.Message, "requested operation was not sent") {
		t.Fatal("lost safe TLS and unsent guidance", failure)
	}
}

func TestTLSVerificationReadClassificationAndMutationUncertainty(t *testing.T) {
	cert := &x509.Certificate{DNSNames: []string{"private.invalid"}}
	for _, cause := range []error{
		&tls.CertificateVerificationError{Err: errors.New("PRIVATE verification failure")},
		x509.UnknownAuthorityError{Cert: cert},
		x509.HostnameError{Certificate: cert, Host: "PRIVATE.invalid"},
		x509.CertificateInvalidError{Cert: cert, Reason: x509.Expired},
		x509.SystemRootsError{Err: errors.New("PRIVATE trust failure")},
	} {
		for _, mutation := range []bool{false, true} {
			c, _ := identityBootstrapFixture(t, 200, "application/json", `{}`)
			calls := 0
			c.http.Transport = identityBootstrapTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, fmt.Errorf("wrapped: %w", cause) })
			method, key := http.MethodGet, ""
			if mutation {
				method, key = http.MethodPost, "original-key"
			}
			err := c.requestWithTimeout(t.Context(), method, []string{"api", "v1", "instance"}, nil, false, key, nil, 200, mutation, new(any), maxResponseBytes, decodeResponse, nil, requestTimeout)
			var failure *Error
			code := "tls_verification_failed"
			if mutation {
				code = "connection_failed"
			}
			if !errors.As(err, &failure) || failure.Code != code || failure.OutcomeUnknown != mutation || calls != 1 {
				t.Fatalf("classification changed mutation safety: %T %#v calls=%d", cause, err, calls)
			}
			if strings.Contains(failure.Message+failure.Action, "PRIVATE") || strings.Contains(failure.Message+failure.Action, "private.invalid") {
				t.Fatal("raw certificate detail leaked", failure)
			}
			if !mutation && (transientIdentityFailure(failure) || resultWaitReconnectFailure(failure)) {
				t.Fatal("certificate failure marked retryable", failure)
			}
		}
	}
}

func TestMessageWaitRejectsUntrustedTLSWithoutReconnect(t *testing.T) {
	var connections, requests atomic.Int64
	service := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	service.Config.ErrorLog = log.New(io.Discard, "", 0)
	service.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	service.StartTLS()
	defer service.Close()
	base, _ := url.Parse(service.URL)
	transport := newTransport()
	defer transport.CloseIdleConnections()
	c := &Client{base: base, transport: transport, http: &http.Client{Transport: transport}, token: "synthetic-private-credential", identity: profile.Context{InstanceID: "instance-one", ProjectID: "project-one", WorkerID: "worker-one"}}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	page, err := c.WaitForMessages(ctx, "saved-cursor", 1, 1)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "tls_verification_failed" || failure.OutcomeUnknown || page.TimedOut || connections.Load() != 1 || requests.Load() != 0 {
		t.Fatal("stream certificate failure retried or hid its cause", page, err, connections.Load(), requests.Load())
	}
	if !strings.Contains(failure.Action, "saved cursor") || !strings.Contains(failure.Action, "messages remain unacknowledged") {
		t.Fatal("lost stream recovery guidance", failure)
	}
}
