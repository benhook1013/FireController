package reviewprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPullMetadataActualGitHubAndRetainedOldCapture(t *testing.T) {
	base := `{"title":"Metadata","number":2,"state":"closed","merged":true,"head":{"sha":"` + strings.Repeat("a", 40) + `","repo":null},"base":{"sha":"` + strings.Repeat("b", 40) + `","ref":"main","repo":{"id":7,"full_name":"owner/repo","default_branch":"main"}},"changed_files":0}`
	var body atomic.Value
	body.Store(base)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/repos/owner/repo/pulls/2" {
			t.Error("unexpected provider work")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body.Load().(string)))
	}))
	defer server.Close()
	g := testGitHub(server, "metadata-fixture")
	old, err := g.Pull(context.Background(), "owner/repo", 2)
	if err != nil || old.Additions != nil || old.Deletions != nil || old.IsDraft != nil || old.MergedAt != "" {
		t.Fatal("old capture facts fabricated", old, err)
	}
	legacy, _ := json.Marshal(old)
	saved := string(legacy)
	if _, err = ValidateHistoryFacts("pull", legacy); err != nil {
		t.Fatal("old normalized capture refused", err)
	}
	if string(legacy) != saved {
		t.Fatal("capture mutated")
	}
	body.Store(strings.TrimSuffix(base, "}") + `,"additions":0,"deletions":0,"draft":false,"merged_at":"2026-01-01T00:00:00.123456789Z"}`)
	known, err := g.Pull(context.Background(), "owner/repo", 2)
	if err != nil || known.Additions == nil || *known.Additions != 0 || known.Deletions == nil || known.IsDraft == nil || *known.IsDraft || known.MergedAt != "2026-01-01T00:00:00.123456789Z" {
		t.Fatal("known zero metadata lost", known, err)
	}
	raw, _ := json.Marshal(known)
	if _, err = ValidateHistoryFacts("pull", raw); err != nil {
		t.Fatal("new normalized capture refused", err)
	}
	for _, extra := range []string{`,"additions":1`, `,"additions":-1,"deletions":0`, `,"additions":2147483648,"deletions":0`, `,"draft":"false"`, `,"merged_at":"invalid"`} {
		body.Store(strings.TrimSuffix(base, "}") + extra + "}")
		if _, err := g.Pull(context.Background(), "owner/repo", 2); err == nil {
			t.Fatal("malformed metadata admitted", extra)
		}
	}
	if calls.Load() != 7 {
		t.Fatal("extra provider requests", calls.Load())
	}
}
