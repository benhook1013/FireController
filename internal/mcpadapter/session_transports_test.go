package mcpadapter_test

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/profile"
)

func TestSessionTransportReuseFreshAuthorityIsolationAndCleanup(t *testing.T) {
	var connections, instances, workers, posts atomic.Int64
	var token, instance atomic.Value
	token.Store("first-private-token")
	instance.Store("instance-one")
	closed := make(chan struct{}, 8)
	service := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token.Load().(string) {
			t.Error("stale credential used")
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			posts.Add(1)
			w.WriteHeader(500)
			return
		}
		if r.URL.Path == "/api/v1/instance" {
			instances.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": instance.Load().(string)})
		} else {
			workers.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "worker-one", "project_id": "project-one", "name": "Worker"})
		}
	}))
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
	credential := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credential, []byte(token.Load().(string)), 0600); err != nil {
		t.Fatal(err)
	}
	identity := localContext()
	identity.ServerURL = service.URL
	server := adapter(t, profiles(t, map[string]profile.Context{"alpha": identity}, credential), "alpha")
	first := connect(t, server, "")
	if call(t, first, "context", nil).IsError {
		t.Fatal("local context failed")
	}
	if connections.Load() != 0 {
		t.Fatal("context opened service transport")
	}
	for i := 0; i < 2; i++ {
		if call(t, first, "doctor", nil).IsError {
			t.Fatal("doctor failed")
		}
	}
	if connections.Load() != 1 || instances.Load() != 2 || workers.Load() != 2 {
		t.Fatal("calls did not reuse connection with fresh instance verification", connections.Load(), instances.Load(), workers.Load())
	}
	second := connect(t, server, "")
	if call(t, second, "doctor", nil).IsError || connections.Load() != 2 {
		t.Fatal("distinct MCP session shared transport")
	}
	token.Store("second-private-token")
	if err := os.WriteFile(credential, []byte(token.Load().(string)), 0600); err != nil {
		t.Fatal(err)
	}
	if call(t, first, "doctor", nil).IsError || connections.Load() != 2 || instances.Load() != 4 {
		t.Fatal("credential rotation was not freshly authenticated")
	}
	if err := os.Remove(credential); err != nil {
		t.Fatal(err)
	}
	errorResult(t, call(t, first, "doctor", nil), "credential_unreadable", credential)
	if instances.Load() != 4 {
		t.Fatal("missing credential reached service")
	}
	if err := os.WriteFile(credential, []byte(token.Load().(string)), 0600); err != nil {
		t.Fatal(err)
	}
	instance.Store("instance-two")
	errorResult(t, call(t, first, "jobs_create", map[string]any{"idempotency_key": "blocked-write", "title": "Blocked"}), "instance_mismatch")
	if posts.Load() != 0 || instances.Load() != 5 {
		t.Fatal("changed instance allowed mutation")
	}
	instance.Store("instance-one")
	if call(t, first, "doctor", nil).IsError || connections.Load() != 2 {
		t.Fatal("borrowed client failure closed shared pool")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("session close retained idle HTTP connection")
	}
	if call(t, second, "doctor", nil).IsError || connections.Load() != 2 {
		t.Fatal("one session close disturbed another pool")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("second session retained idle HTTP connection")
	}
}
