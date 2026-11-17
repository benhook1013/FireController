package reviewprovider

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func targetLogs(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()
	var logs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		if line == "" {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			t.Fatal(err)
		}
		if item["msg"] == "GitHub target request totals" {
			logs = append(logs, item)
		}
	}
	return logs
}

func TestGitHubTargetAttributionRotatesWithoutProviderMaterial(t *testing.T) {
	var output bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	q := NewGitHubQuota()
	now := time.Now()
	ctx := WithGitHubTarget(WithGitHubCaller(t.Context(), "lifecycle_enrichment_hint"), 42, 7)
	for range 3 {
		q.observe(ctx, now, "/repos/secret-owner/secret-name/commits/secret-sha/check-runs", &http.Response{StatusCode: 304}, true, false)
	}
	q.observe(ctx, now, "/private-secret-path", nil, false, true)
	q.observe(t.Context(), now.Add(time.Minute), "/user", nil, false, false)
	logs := targetLogs(t, &output)
	if len(logs) != 1 {
		t.Fatalf("target windows=%d", len(logs))
	}
	var rows []githubTargetRow
	if err := json.Unmarshal([]byte(logs[0]["targets"].(string)), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Repository != 42 || rows[0].Pull != 7 || rows[0].Total != (githubTargetCount{Requests: 3, Conditional: 3, NotModified: 3, Gated: 1}) {
		t.Fatalf("wrong attribution: %+v", rows)
	}
	if rows[0].Callers["lifecycle_enrichment_hint"] != rows[0].Total {
		t.Fatal("lost caller counts")
	}
	if len(q.targets.rows) != 0 || strings.Contains(output.String(), "secret") {
		t.Fatal("retained expired window or leaked provider text")
	}
}

func TestGitHubTargetAttributionBoundsAndReportsOmissions(t *testing.T) {
	var output bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	q := NewGitHubQuota()
	now := time.Now()
	for n := int64(1); n <= 70; n++ {
		q.observe(WithGitHubTarget(t.Context(), 42, n), now, "/user", nil, false, false)
	}
	hot := WithGitHubTarget(t.Context(), 42, 64)
	for range 9 {
		q.observe(hot, now, "/user", nil, false, false)
	}
	if len(q.targets.rows) != 64 || q.targets.overflow.Requests != 6 {
		t.Fatal("unbounded targets or lost overflow")
	}
	q.observe(t.Context(), now.Add(time.Minute), "/user", nil, false, false)
	logs := targetLogs(t, &output)
	if len(logs) != 1 {
		t.Fatal("expected one bounded report")
	}
	var rows []githubTargetRow
	if err := json.Unmarshal([]byte(logs[0]["targets"].(string)), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 16 || rows[0].Pull != 64 || rows[0].Total.Requests != 10 {
		t.Fatal("heavy target missing")
	}
	omitted := logs[0]["omitted_target_totals"].(map[string]any)
	overflow := logs[0]["overflow_totals"].(map[string]any)
	if omitted["requests"] != float64(48) || overflow["requests"] != float64(6) {
		t.Fatal("omissions not truthful")
	}
}

func TestGitHubTargetAttributionInvalidScopeAndConcurrentReads(t *testing.T) {
	q := NewGitHubQuota()
	now := time.Now()
	for _, pair := range [][2]int64{{0, 1}, {1, 0}, {-1, 1}, {1, -1}} {
		q.observe(WithGitHubTarget(t.Context(), pair[0], pair[1]), now, "/user", nil, false, false)
	}
	if len(q.targets.rows) != 0 {
		t.Fatal("invalid target retained")
	}
	ctx := WithGitHubTarget(WithGitHubCaller(t.Context(), "lifecycle_metadata"), 42, 1)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				q.observe(ctx, now, "/user", nil, false, false)
			}
		})
	}
	wg.Wait()
	if len(q.targets.rows) != 1 || q.targets.rows[githubTargetKey{42, 1}].Total.Requests != 800 {
		t.Fatal("concurrent counts lost")
	}
	// A backwards wall clock cannot cause a duplicate flush or cross-window reuse.
	q.observe(ctx, now.Add(-time.Second), "/user", nil, false, false)
	if q.targets.rows[githubTargetKey{42, 1}].Total.Requests != 801 {
		t.Fatal("backwards clock lost counts")
	}
}

func TestGitHubTargetReportHasStrictPayloadBudget(t *testing.T) {
	var output bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	var window githubTargetWindow
	callers := []string{"review_selection", "cli_preparation", "hosted_preparation", "hosted_recovery", "lifecycle_metadata", "lifecycle_enrichment", "lifecycle_enrichment_hint", "lifecycle_enrichment_comparison", "lifecycle_enrichment_periodic", "ci_watch", "source_refresh", "history_import"}
	for target := int64(1); target <= githubTargetCapacity; target++ {
		for _, caller := range callers {
			window.observe(WithGitHubTarget(t.Context(), 42, target), caller, true, false, 304)
		}
	}
	window.report(t.Context(), "synthetic", time.Now())
	logs := targetLogs(t, &output)
	if len(logs) != 1 {
		t.Fatal("missing bounded report")
	}
	payload := logs[0]["targets"].(string)
	if len(payload) > githubTargetReportBytes {
		t.Fatal("payload exceeded fixed byte budget")
	}
	var rows []githubTargetRow
	if err := json.Unmarshal([]byte(payload), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) >= githubTargetReportLimit {
		t.Fatal("fixture failed to exercise byte-bound trimming")
	}
	count := int(logs[0]["omitted_target_totals"].(map[string]any)["requests"].(float64))
	for _, row := range rows {
		count += row.Total.Requests
	}
	if count != githubTargetCapacity*len(callers) {
		t.Fatal("byte trimming lost omission accounting")
	}
}
