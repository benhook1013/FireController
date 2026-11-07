package client_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

func TestTransportPoolBorrowedCloseAndOwnedDefault(t *testing.T) {
	var connections atomic.Int64
	closed := make(chan struct{}, 8)
	service := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handshake(w) }))
	service.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	service.Start()
	defer service.Close()
	b := binding(t, service.URL)
	pool := client.NewTransportPool()
	defer pool.Close()
	for i := 0; i < 2; i++ {
		c, err := client.NewWithTransportPool(context.Background(), b, pool)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	if connections.Load() != 1 {
		t.Fatal("borrowed clients discarded pooled connection", connections.Load())
	}
	pool.Close()
	pool.Close()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("pool close retained idle connection")
	}
	_, err := client.NewWithTransportPool(context.Background(), b, pool)
	checkError(t, err, "canceled")
	if connections.Load() != 1 {
		t.Fatal("closed pool opened connection")
	}
	c, err := client.New(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("default owned client retained idle connection")
	}
	if connections.Load() != 2 {
		t.Fatal("default client borrowed unrelated pool")
	}
}

func TestTransportPoolPendingStreamsDoNotOccupyFiniteSlots(t *testing.T) {
	entered, detached := make(chan struct{}, 5), make(chan struct{}, 5)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		entered <- struct{}{}
		<-r.Context().Done()
		detached <- struct{}{}
	}))
	defer service.Close()
	pool := client.NewTransportPool()
	defer pool.Close()
	b := binding(t, service.URL)
	c, err := client.NewWithTransportPool(t.Context(), b, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 5)
	for i := 0; i < 5; i++ {
		go func() { _, err := c.WaitForMessages(ctx, "selected-cursor", 5, 30); done <- err }()
	}
	for i := 0; i < 5; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			cancel()
			t.Fatal("stream occupied shared finite connection slots")
		}
	}
	finite, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	another, err := client.NewWithTransportPool(finite, b, pool)
	if err != nil {
		cancel()
		t.Fatal("finite instance check blocked behind streams", err)
	}
	another.Close()
	cancel()
	for i := 0; i < 5; i++ {
		select {
		case err := <-done:
			checkError(t, err, "canceled")
		case <-time.After(time.Second):
			t.Fatal("stream did not cancel")
		}
		select {
		case <-detached:
		case <-time.After(time.Second):
			t.Fatal("canceled stream retained server request")
		}
	}
}
