package reviewexec

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
)

// Exercise a quiet CI response beyond the daemon's ordinary HTTP deadlines.
// The fixture owns no provider worker: unknown must stay unknown at timeout.
func TestCIWatchQuietRealHTTPTimeoutBoundary(t *testing.T) {
	f, s, _, reader, _ := ciWatchFixture(t)
	admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'quiet-reader','Reader')`, attemptProject)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			core.WriteJSON(w, 200, map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	srv.Config.ReadHeaderTimeout = 5 * time.Second
	srv.Config.ReadTimeout = 10 * time.Second
	srv.Config.WriteTimeout = 10 * time.Second
	srv.Config.IdleTimeout = 30 * time.Second
	srv.Start()
	defer srv.Close()
	credential := filepath.Join(t.TempDir(), "reader.token")
	if err := os.WriteFile(credential, []byte(f.reader), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(t.Context(), profile.Binding{Context: profile.Context{Profile: "fixture", ServerURL: srv.URL, InstanceID: f.store.InstanceID(), ProjectID: attemptProject, WorkerID: "quiet-reader"}, CredentialFile: credential})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	started := time.Now()
	value, err := c.WatchCI(t.Context(), []client.CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}}, 12)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("elapsed=%s error=%v", elapsed, err)
	}
	if value.State != "pending" || !value.TimedOut || len(value.Targets) != 1 || value.Targets[0].Reason != "ci_watch_check_pending" || value.Targets[0].ObservedAt != "" {
		t.Fatalf("unexpected quiet result: %+v", value)
	}
	if elapsed < 12*time.Second || elapsed > 16*time.Second {
		t.Fatalf("budget mismatch %s", elapsed)
	}
	ciWatchRegistered(t, s, 0)
	reader.mu.Lock()
	calls := reader.calls
	reader.mu.Unlock()
	if calls != 0 {
		t.Fatalf("provider calls=%d", calls)
	}
	t.Logf("real HTTP/1 strict client returned pending timed_out after %s with daemon-equivalent ReadTimeout/WriteTimeout10s; no provider calls", elapsed)
}
