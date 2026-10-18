package publicsite

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func TestPublicStreamsThroughOneProxyUseAggregateCapacityAndRelease(t *testing.T) {
	f := newPublicationFixture(t)
	if w := f.apply(t, Approved{0, true, testPublicManifest()}, "publish"); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	first := f.public(t)
	before := publicationCounts(t, f.store)
	finished := make(chan struct{}, 257)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { finished <- struct{}{} }()
		f.mux.ServeHTTP(w, r)
	}))
	defer backend.Close()
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(httputil.NewSingleHostReverseProxy(target))
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	responses := make([]*http.Response, 0, 256)
	defer func() {
		for _, response := range responses {
			response.Body.Close()
		}
	}()
	open := func() *http.Response {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, proxy.URL+"/api/v1/public/projects/showcase/events?cursor="+first.Cursor, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := proxy.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	// All upstream requests have the same loopback proxy address. There is no
	// trusted viewer identity in RemoteAddr or arbitrary forwarding headers.
	for i := range 256 {
		response := open()
		responses = append(responses, response)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("stream %d refused through shared proxy: %d", i+1, response.StatusCode)
		}
		frame, err := readPublicFrame(bufio.NewReader(response.Body))
		if err != nil || frame != ": ready\n\n" {
			t.Fatal("stream readiness", i+1, frame, err)
		}
	}
	if f.service.streams.Load() != 256 {
		t.Fatal("streams not held at aggregate capacity")
	}
	overloaded := open()
	overloaded.Body.Close()
	if overloaded.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("aggregate stream bound not enforced", overloaded.StatusCode)
	}
	select {
	case <-finished: // Refused request has released its transient counter.
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	for _, response := range responses {
		response.Body.Close()
	}
	cleanup := time.NewTimer(3 * time.Second)
	defer cleanup.Stop()
	for range 256 {
		select {
		case <-finished:
		case <-cleanup.C:
			t.Fatal("canceled proxy stream did not release")
		}
	}
	if f.service.streams.Load() != 0 {
		t.Fatal("stream accounting leaked")
	}
	// Refill the store's independent subscription bound to prove that every
	// canceled HTTP stream also released its underlying waiter.
	releases := make([]func(), 0, 1024)
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for i := range 1024 {
		_, release, err := f.store.SubscribePublicProjection(f.project, fmt.Sprintf("cleanup_%d", i))
		if err != nil {
			t.Fatal("underlying waiter leaked", i, err)
		}
		releases = append(releases, release)
	}
	if _, release, err := f.store.SubscribePublicProjection(f.project, "over_capacity"); err == nil {
		release()
		t.Fatal("store aggregate bound not enforced")
	} else if failure, ok := err.(*core.Error); !ok || failure.Code != "overloaded" {
		t.Fatal("unexpected capacity error", err)
	}
	if !reflectCounts(before, publicationCounts(t, f.store)) {
		t.Fatal("anonymous streams changed private state")
	}
}
