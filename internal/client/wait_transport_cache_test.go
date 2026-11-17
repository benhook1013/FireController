package client

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitTransportCacheExclusiveBoundedAndExactSettings(t *testing.T) {
	base := newTransport()
	base.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "fixture"}
	defer base.CloseIdleConnections()
	cache := &waitTransportCache{}
	defer cache.close()
	a, releaseA := cache.lease(base, 35*time.Second)
	b, releaseB := cache.lease(base, 35*time.Second)
	if a == b || a == base || a.ResponseHeaderTimeout != 35*time.Second || base.ResponseHeaderTimeout != requestTimeout || a.TLSHandshakeTimeout != base.TLSHandshakeTimeout || a.MaxConnsPerHost != base.MaxConnsPerHost || a.IdleConnTimeout != base.IdleConnTimeout || a.DisableCompression != base.DisableCompression || a.Proxy == nil || reflect.ValueOf(a.Proxy).Pointer() != reflect.ValueOf(base.Proxy).Pointer() || a.TLSClientConfig.MinVersion != base.TLSClientConfig.MinVersion || a.TLSClientConfig.ServerName != base.TLSClientConfig.ServerName || a.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("lease changed settings or shared active transport")
	}
	releaseA()
	releaseA()
	c, releaseC := cache.lease(base, 35*time.Second)
	if c != a {
		t.Fatal("matching timeout not reused")
	}
	releaseC()
	releaseB()
	transports := map[*http.Transport]bool{}
	for i := 0; i < maxIdleWaitTransports+2; i++ {
		tr, release := cache.lease(base, time.Duration(40+i)*time.Second)
		transports[tr] = true
		release()
	}
	cache.mu.Lock()
	if len(cache.idle) != maxIdleWaitTransports {
		t.Error("idle bound not enforced", len(cache.idle))
	}
	for _, v := range cache.idle {
		if !transports[v.transport] || v.transport.ResponseHeaderTimeout != v.timeout {
			t.Error("retained wrong transport")
		}
	}
	cache.mu.Unlock()
	tr, release := cache.lease(base, 99*time.Second)
	cache.close()
	release()
	cache.close()
	after, afterRelease := cache.lease(base, 99*time.Second)
	if after == tr {
		t.Fatal("closed owner retained late release")
	}
	afterRelease()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.idle) != 0 {
		t.Fatal("closed cache repopulated")
	}
}

func TestWaitTransportCacheConcurrentLeaseReleaseAndClose(t *testing.T) {
	base := newTransport()
	defer base.CloseIdleConnections()
	cache := &waitTransportCache{}
	var wg sync.WaitGroup
	var mu sync.Mutex
	active := map[*http.Transport]bool{}
	for range 20 {
		wg.Go(func() {
			for range 20 {
				tr, release := cache.lease(base, 35*time.Second)
				mu.Lock()
				if active[tr] {
					t.Error("active lease reused")
				}
				active[tr] = true
				mu.Unlock()
				mu.Lock()
				delete(active, tr)
				mu.Unlock()
				release()
			}
		})
	}
	wg.Go(cache.close)
	wg.Wait()
	cache.close()
}

func TestWaitTransportCacheEvictionClosesIdleSocketsAndLateRelease(t *testing.T) {
	var closed atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	base := newTransport()
	defer base.CloseIdleConnections()
	cache := &waitTransportCache{}
	defer cache.close()
	use := func(timeout time.Duration) func() {
		tr, release := cache.lease(base, timeout)
		response, err := (&http.Client{Transport: tr, Timeout: time.Second}).Get(srv.URL)
		if err != nil {
			release()
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if err != nil {
			release()
			t.Fatal(err)
		}
		return release
	}
	await := func(want int64) {
		until := time.Now().Add(2 * time.Second)
		for closed.Load() != want && time.Now().Before(until) {
			time.Sleep(time.Millisecond)
		}
		if closed.Load() != want {
			t.Fatalf("closed sockets=%d want%d", closed.Load(), want)
		}
	}
	for i := 0; i < maxIdleWaitTransports+1; i++ {
		use(time.Duration(10+i) * time.Second)()
	}
	await(1)
	lateRelease := use(99 * time.Second)
	cache.close()
	await(maxIdleWaitTransports + 1)
	lateRelease()
	await(maxIdleWaitTransports + 2)
}
