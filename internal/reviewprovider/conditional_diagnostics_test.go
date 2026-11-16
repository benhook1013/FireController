package reviewprovider

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConditionalDiagnosticsSeparateAdmissionReasons(t *testing.T) {
	g := &GitHub{token: "synthetic", quota: NewGitHubQuota()}
	cases := []struct{ name, etag, control, body string }{
		{"none", "", "", `{}`}, {"no-store", `"valid"`, "private, no-store", `{}`},
		{"bad-etag", "invalid\n", "", `{}`}, {"bad-json", `"valid"`, "", `bad`},
		{"stored", `"valid"`, "", `{}`},
	}
	for _, c := range cases {
		r, _ := http.NewRequest(http.MethodGet, "https://api.github.com/repos/PRIVATE/repo/actions/runs/"+c.name, nil)
		g.conditionalRequest(r)
		g.rememberConditional(r, &http.Response{StatusCode: 200, Header: http.Header{"Etag": []string{c.etag}, "Cache-Control": []string{c.control}}}, []byte(c.body))
	}
	got := g.conditionalCounts["actions_current_run"]
	if got.Misses != 5 || got.Responses200 != 5 || got.ETagPresent != 4 || got.NoETag != 1 || got.NoStore != 1 || got.InvalidETag != 1 || got.InvalidJSON != 1 || got.Stored != 1 || len(g.cache) != 1 {
		t.Fatal(got, len(g.cache))
	}
	r, _ := http.NewRequest(http.MethodGet, "https://api.github.com/repos/PRIVATE/repo/actions/runs/stored", nil)
	if _, ok := g.conditionalRequest(r); !ok || g.conditionalCounts["actions_current_run"].Hits != 1 {
		t.Fatal("hit lost")
	}
}

func TestConditionalDiagnosticsSeparateByteAndEntryEvictions(t *testing.T) {
	for _, mode := range []string{"bytes", "entries"} {
		t.Run(mode, func(t *testing.T) {
			g := &GitHub{token: "synthetic", quota: NewGitHubQuota()}
			count, body := conditionalMaxEntries+1, []byte(`{}`)
			header := http.Header{"Etag": []string{`"same"`}}
			if mode == "bytes" {
				count = 4
				header.Set("X-Budget-Fixture", strings.Repeat("x", 5<<20))
			}
			for i := 0; i < count; i++ {
				r, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("https://api.github.com/repos/PRIVATE/repo/actions/runs/%d", i), nil)
				g.rememberConditional(r, &http.Response{StatusCode: 200, Header: header}, body)
			}
			got := g.conditionalCounts["actions_current_run"]
			if got.Evictions != 1 || (mode == "bytes") != (got.BytePressure == 1) || (mode == "entries") != (got.EntryPressure == 1) || g.cacheBytes > conditionalMaxBytes || len(g.cache) > conditionalMaxEntries {
				t.Fatal(got)
			}
		})
	}
}

func TestConditionalDiagnosticsAreBoundedConcurrentAndPrivate(t *testing.T) {
	var out bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	defer slog.SetDefault(old)
	g := &GitHub{token: "PRIVATE_TOKEN", quota: NewGitHubQuota()}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Go(func() { g.recordConditional("/repos/PRIVATE/repo/actions/runs/PRIVATE_ID", "hit") })
	}
	wg.Wait()
	if len(g.conditionalCounts) != 1 || g.conditionalCounts["actions_current_run"].Hits != 100 {
		t.Fatal(g.conditionalCounts)
	}
	g.reportConditional(context.Background(), time.Now().Add(2*time.Minute))
	if len(g.conditionalCounts) != 0 || strings.Contains(out.String(), "PRIVATE") || !strings.Contains(out.String(), "actions_current_run") {
		t.Fatal("unbounded/private/stale diagnostic", out.String())
	}
}
