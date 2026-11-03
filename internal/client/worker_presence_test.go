package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestClientConnectionIdentityIsStablePrivateAndDistinct(t *testing.T) {
	var mu sync.Mutex
	sessions := []string{}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			if r.Header.Get("X-FC-Session") != "" {
				t.Error("unscoped handshake attributed a worker connection")
			}
			handshake(w)
			return
		}
		id := r.Header.Get("X-FC-Session")
		if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(id) {
			t.Error("invalid client connection identity")
		}
		mu.Lock()
		sessions = append(sessions, id)
		mu.Unlock()
		write(w, 200, map[string]any{"items": []any{}, "has_more": false})
	}))
	defer fixture.Close()
	first := newClient(t, fixture)
	defer first.Close()
	second := newClient(t, fixture)
	defer second.Close()
	pool := client.NewTransportPool()
	defer pool.Close()
	pooledFirst, err := client.NewWithTransportPool(context.Background(), binding(t, fixture.URL), pool)
	if err != nil {
		t.Fatal(err)
	}
	defer pooledFirst.Close()
	pooledSecond, err := client.NewWithTransportPool(context.Background(), binding(t, fixture.URL), pool)
	if err != nil {
		t.Fatal(err)
	}
	defer pooledSecond.Close()
	for _, c := range []*client.Client{first, first, second, pooledFirst, pooledSecond} {
		if _, err := c.Workers(context.Background(), client.PageOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sessions) != 5 || sessions[0] != sessions[1] || sessions[0] == sessions[2] || sessions[3] != sessions[4] || sessions[3] == sessions[0] || sessions[3] == sessions[2] {
		t.Fatal("client connection identity is not stable and isolated")
	}
	if first.Identity() != second.Identity() {
		t.Fatal("transport identity changed the bound profile")
	}
}
