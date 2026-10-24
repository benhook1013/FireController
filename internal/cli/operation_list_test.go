package cli_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestOperationListCLILocalHelpAndStrictArguments(t *testing.T) {
	for _, args := range [][]string{{"reviews", "operations", "--help"}, {"help", "reviews", "operations"}} {
		var out, stderr bytes.Buffer
		code := cli.Run(append([]string{"--profiles-file", "missing-private-profiles", "--profile", "absent"}, args...), nil, &out, &stderr)
		if code != 0 || stderr.Len() != 0 || !strings.Contains(out.String(), "docs/review-operations.md") || !strings.Contains(out.String(), "--all") {
			t.Fatal(code, out.String(), stderr.String())
		}
	}
	for _, flags := range [][]string{{"42"}, {"--pr", "0"}, {"--pr", "01"}, {"--pr", "2147483648"}, {"--channel", "other"}, {"--channel", ""}, {"--all=true"}, {"--all", "--all"}, {"--limit", "0"}, {"--limit", "101"}, {"--limit", "05"}, {"--limit", "5", "--limit", "1"}, {"--cursor", ""}, {"--cursor", " bad"}, {"--status", "all"}, {"--all", "--help"}} {
		var out, stderr bytes.Buffer
		args := append([]string{"--profiles-file", "missing-private-profiles", "reviews", "operations"}, flags...)
		code := cli.Run(args, nil, &out, &stderr)
		if code != cli.ExitUsage || out.Len() != 0 || !strings.Contains(stderr.String(), "reviews operations --help") || strings.Contains(stderr.String(), "credential") {
			t.Fatal(flags, code, out.String(), stderr.String())
		}
	}
}

func TestOperationListCLIRealHandlerDefaultsAllFilterAndRecovery(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t)
	var lists atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reviews/operations") {
			lists.Add(1)
			if r.Method != "GET" || r.Header.Get("X-FC-Worker") != worker || r.Header.Get("X-FC-Instance") != f.store.InstanceID() {
				t.Error("operation list lost binding")
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	providers := f.resolverHit.Load()
	first := mutationSuccess(t, path, "builder", reviewRequestArgs()...)
	id := first["id"].(string)
	read := func(flags ...string) client.ReviewOperationsPage {
		t.Helper()
		code, out, stderr := mutationInvoke(t, path, "builder", append([]string{"reviews", "operations"}, flags...)...)
		var page client.ReviewOperationsPage
		if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &page) != nil {
			t.Fatal(code, out, stderr)
		}
		return page
	}
	page := read()
	if page.ProjectID != project || len(page.Items) != 1 || page.Items[0].ID != id || page.Items[0].WorkerID != worker || page.HasMore {
		t.Fatal(page)
	}
	if _, err := f.store.DB().Exec(`UPDATE review_operations SET state='completed' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if page := read(); len(page.Items) != 0 || page.Items == nil {
		t.Fatal(page)
	}
	page = read("--pr", "2", "--channel", "cli", "--all", "--limit", "1")
	if len(page.Items) != 1 || page.Items[0].ID != id || page.Items[0].State != "completed" {
		t.Fatal(page)
	}
	if page := read("--channel", "hosted", "--all"); len(page.Items) != 0 {
		t.Fatal(page)
	}
	if lists.Load() != 4 || f.resolverHit.Load() != providers {
		t.Fatal("list retried, paginated or contacted provider", lists.Load())
	}
}
