package cli_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func cliAdoptionControl() client.PreparedReviewRoundControl {
	zero := int64(0)
	return client.PreparedReviewRoundControl{Version: 1, RepositoryID: 1, ExpectedRevision: &zero, PullNumber: 2, Channel: "cli", Source: "sealed source projection", SourceKey: "2:cli", SourceSHA256: strings.Repeat("a", 64), MaximumRemaining: 4, InitialDry: 2, Taper: 3}
}

func TestRoundAdoptionCLIActualPreviewReplayAndRead(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t)
	var posts atomic.Int64
	var drop atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/adoption/apply") && drop.Swap(false) {
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, r)
			if rec.Code != 200 {
				t.Errorf("adoption did not commit %d %s", rec.Code, rec.Body.String())
			}
			conn, _, e := w.(http.Hijacker).Hijack()
			if e == nil {
				_ = conn.Close()
			}
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	profiles := mutationProfiles(t, f, server.URL, map[string][2]string{"admin": {project, worker}})
	control := cliAdoptionControl()
	raw, _ := json.Marshal(control)
	file := importCLIFile(t, raw)
	count := func() string {
		var s string
		if e := f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM review_round_controls)||':'||(SELECT count(*) FROM review_round_adoptions)||':'||(SELECT count(*) FROM review_round_changes)||':'||(SELECT count(*) FROM review_operations)||':'||(SELECT count(*) FROM review_round_completions)||':'||(SELECT count(*) FROM events)||':'||(SELECT count(*) FROM receipts)`).Scan(&s); e != nil {
			t.Fatal(e)
		}
		return s
	}
	before := count()
	provider := f.resolverHit.Load()
	code, out, stderr := mutationInvoke(t, profiles, "admin", "reviews", "rounds", "adopt", "preview", "--file", file)
	var preview client.ReviewRoundAdoptionPreview
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &preview) != nil || preview.ProjectID != project || count() != before || strings.Contains(out, control.Source) {
		t.Fatal("preview expanded/mutated", code, out, stderr)
	}
	args := []string{"reviews", "rounds", "adopt", "apply", "--file", file, "--preview", preview.PreviewSHA256, "--key", "carry-cli-two-dry"}
	drop.Store(true)
	code, out, stderr = mutationInvoke(t, profiles, "admin", args...)
	if code == 0 || out != "" || !strings.Contains(stderr, `"outcome_unknown":true`) || posts.Load() != 2 {
		t.Fatal("lost result retried/hidden", code, stderr, posts.Load())
	}
	code, out, stderr = mutationInvoke(t, profiles, "admin", args...)
	var receipt client.ReviewRoundControlReceipt
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &receipt) != nil || receipt.Outcome != "adopted" || receipt.Revision != 1 {
		t.Fatal("exact result recovery failed", code, stderr)
	}
	committed := count()
	code, out, stderr = mutationInvoke(t, profiles, "admin", "reviews", "rounds", "read", "--pr", "2", "--channel", "cli")
	var value client.ReviewRoundControl
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &value) != nil || value.InitialDry != 2 || value.Dry != 2 || value.Used != 0 || value.Maximum != 4 || value.OriginSHA256 != control.SourceSHA256 || count() != committed {
		t.Fatal("carried control lost provenance/created credit", code, out, stderr)
	}
	// A later ordinary grant clears the seed, but replay of the original adoption
	// remains its original receipt and cannot undo that new operational decision.
	code, _, stderr = mutationInvoke(t, profiles, "admin", "reviews", "rounds", "grant", "--pr", "2", "--channel", "cli", "--expected-revision", "1", "--max", "7", "--key", "new-native-grant")
	if code != 0 || stderr != "" {
		t.Fatal("replacement grant failed", code, stderr)
	}
	afterGrant := count()
	code, out, stderr = mutationInvoke(t, profiles, "admin", args...)
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &receipt) != nil || receipt.Revision != 1 || count() != afterGrant {
		t.Fatal("adoption replay undid later grant", code, stderr)
	}
	var diagnostic strings.Builder
	full := append([]string{"--profiles-file", profiles, "--profile", "admin"}, args...)
	code = cli.RunContext(context.Background(), full, nil, selectionFailWriter{}, &diagnostic)
	if code != cli.ExitConfig || !strings.Contains(diagnostic.String(), `"outcome_unknown":true`) || count() != afterGrant {
		t.Fatal("lost stdout recovery failed", code, diagnostic.String())
	}
	if f.resolverHit.Load() != provider {
		t.Fatal("local adoption/read touched provider")
	}
}

func TestRoundAdoptionCLIFileAndSyntaxBeforeCredentials(t *testing.T) {
	for _, args := range [][]string{
		{"reviews", "rounds", "adopt"}, {"reviews", "rounds", "adopt", "preview", "--file", "relative"}, {"reviews", "rounds", "adopt", "preview", "--file", "/missing", "--key", "forbidden"}, {"reviews", "rounds", "adopt", "apply", "--file", "/missing"}, {"reviews", "rounds", "adopt", "preview", "--file", "/missing", "--file", "/other"},
	} {
		code, out, stderr := run(append([]string{"--profiles-file", "/missing/profile"}, args...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal(args, code, stderr)
		}
	}
	valid, _ := json.Marshal(cliAdoptionControl())
	dir := t.TempDir()
	file := filepath.Join(dir, "control.json")
	for _, raw := range [][]byte{[]byte("null"), []byte(strings.Repeat(" ", 16<<10) + string(valid)), []byte(`{"version":1}`)} {
		if e := os.WriteFile(file, raw, 0600); e != nil {
			t.Fatal(e)
		}
		code, out, stderr := run([]string{"--profiles-file", "/missing/profile", "reviews", "rounds", "adopt", "preview", "--file", file}, nil)
		if code == 0 || out != "" || strings.Contains(stderr, "profile") {
			t.Fatal("invalid file reached credential lookup", code, stderr)
		}
	}
	if runtime.GOOS != "windows" {
		if e := os.WriteFile(file, valid, 0644); e != nil {
			t.Fatal(e)
		}
		if e := os.Chmod(file, 0644); e != nil {
			t.Fatal(e)
		}
		code, _, stderr := run([]string{"reviews", "rounds", "adopt", "preview", "--file", file}, nil)
		if code == 0 || !strings.Contains(stderr, "invalid_import_file") {
			t.Fatal("world readable prepared control accepted", code, stderr)
		}
	}
}

func TestRoundAdoptionCLIV2UnboundedReadAndExplicitFiniteReplacement(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t)
	server := httptest.NewServer(f.handler)
	defer server.Close()
	profiles := mutationProfiles(t, f, server.URL, map[string][2]string{"admin": {project, worker}})
	p := cliAdoptionControl()
	p.Version = 2
	p.MaximumUnbounded = true
	p.MaximumRemaining = 0
	p.MinimumRemaining = 1001
	raw, _ := json.Marshal(p)
	file := importCLIFile(t, raw)
	before := f.resolverHit.Load()
	code, out, stderr := mutationInvoke(t, profiles, "admin", "reviews", "rounds", "adopt", "preview", "--file", file)
	var preview client.ReviewRoundAdoptionPreview
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &preview) != nil {
		t.Fatal("v2 preview refused", code, stderr)
	}
	code, _, stderr = mutationInvoke(t, profiles, "admin", "reviews", "rounds", "adopt", "apply", "--file", file, "--preview", preview.PreviewSHA256, "--key", "unbounded-source")
	if code != 0 || stderr != "" {
		t.Fatal("v2 apply refused", code, stderr)
	}
	code, out, stderr = mutationInvoke(t, profiles, "admin", "reviews", "rounds", "read", "--pr", "2", "--channel", "cli")
	var got client.ReviewRoundControl
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &got) != nil || !got.MaximumUnbounded || got.Maximum != 0 || got.Minimum != 1001 || got.Used != 0 {
		t.Fatal("read invented finite maximum", code, stderr)
	}
	code, _, stderr = mutationInvoke(t, profiles, "admin", "reviews", "rounds", "grant", "--pr", "2", "--channel", "cli", "--expected-revision", "1", "--max", "5", "--key", "finite-again")
	if code != 0 || stderr != "" {
		t.Fatal("finite replacement refused", code, stderr)
	}
	code, out, stderr = mutationInvoke(t, profiles, "admin", "reviews", "rounds", "read", "--pr", "2", "--channel", "cli")
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &got) != nil || strings.Contains(out, "maximum_unbounded") || got.Maximum != 5 {
		t.Fatal("finite read retained unbounded flag", code, stderr)
	}
	if f.resolverHit.Load() != before {
		t.Fatal("configuration contacted provider")
	}
}
