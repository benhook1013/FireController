package client

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/profile"
)

// A reset HTTP/2 stream returns a transport StreamError that is neither EOF
// nor net.Error. It must resume from validated readiness, not fail validation.
func TestMessageWaitHTTP2ResetResumesValidatedCursor(t *testing.T) {
	var streams atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := streams.Add(1)
		cursor := "saved-cursor"
		if n > 1 {
			cursor = "validated-cursor"
		}
		if r.ProtoMajor != 2 || r.Method != "GET" || r.URL.Query().Get("cursor") != cursor || r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Error("reconnect changed protocol, cursor or authority", r.Proto, r.URL)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: message-events\nid: validated-cursor\ndata: {\"items\":[],\"cursor\":\"validated-cursor\",\"has_more\":false,\"timed_out\":false}\n\n")
		w.(http.Flusher).Flush()
		if n == 1 {
			panic(http.ErrAbortHandler)
		}
		<-r.Context().Done()
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	transport := newTransport()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool}
	base, _ := url.Parse(server.URL)
	c := &Client{base: base, identity: profile.Context{InstanceID: "instance-one", ProjectID: "project-one", WorkerID: "worker-one"}, token: "synthetic-token", credentialFile: "synthetic-credential-path", transport: transport, http: &http.Client{Transport: transport}}
	defer c.Close()
	page, err := c.WaitForMessages(t.Context(), "saved-cursor", 1, 1)
	if err != nil || !page.TimedOut || page.Cursor != "validated-cursor" || streams.Load() != 2 {
		t.Fatal("HTTP/2 reset was not reconnected from validated cursor", page, err, streams.Load())
	}
}
