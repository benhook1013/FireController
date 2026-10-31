package cli_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
)

func TestCLIRecoversTimedOutIdentityBeforeSendingOneJobMutation(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "identity-recovery")
	worker := f.worker(t, project, "Builder")
	var identities, writes atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" && identities.Add(1) == 1 {
			<-r.Context().Done() // Exercise the actual five-second HTTP timeout.
			return
		}
		if r.Method != "GET" {
			writes.Add(1)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	path := mutationProfiles(t, f, s.URL, map[string][2]string{"builder": {project, worker}})
	start := time.Now()
	code, out, stderr := mutationInvoke(t, path, "builder", "jobs", "create", "--title", "Recovered request", "--key", "original-job-key")
	if code != cli.ExitSuccess || stderr != "" || !strings.Contains(out, `"outcome":"created"`) || identities.Load() != 2 || writes.Load() != 1 || time.Since(start) > 10*time.Second {
		t.Fatal("CLI bootstrap consumed operation budget or duplicated work", code, out, stderr, identities.Load(), writes.Load())
	}
}
