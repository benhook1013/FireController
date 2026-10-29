package mcpadapter_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestMCPRecoversTimedOutIdentityBeforeSendingOneJobMutation(t *testing.T) {
	var identities, writes atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			if identities.Add(1) == 1 {
				<-r.Context().Done()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": "instance-one"})
			return
		}
		writes.Add(1)
		if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/jobs" || r.Header.Get("Idempotency-Key") != "original-job-key" || r.Header.Get("X-FC-Worker") != "worker-one" {
			t.Error("recovery changed or repeated selected operation")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(client.JobReceipt{ID: "job-one", Outcome: "created", Revision: 1})
	}))
	defer s.Close()
	identity := localContext()
	identity.ServerURL = s.URL
	credential := filepath.Join(t.TempDir(), "credential.token")
	if err := os.WriteFile(credential, []byte("synthetic-private-token"), 0600); err != nil {
		t.Fatal(err)
	}
	p := profiles(t, map[string]profile.Context{"alpha": identity}, credential)
	session := connect(t, adapter(t, p, "alpha"), "2025-06-18")
	start := time.Now()
	result := call(t, session, "jobs_create", map[string]any{"title": "Recovered request", "idempotency_key": "original-job-key"})
	var receipt client.JobReceipt
	decodeResult(t, result, &receipt)
	if receipt.ID != "job-one" || identities.Load() != 2 || writes.Load() != 1 || time.Since(start) > 10*time.Second {
		t.Fatal("MCP bootstrap consumed operation budget or duplicated work", receipt, identities.Load(), writes.Load())
	}
}
