package reviews

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestNativeAttemptGrowthRetainsUnrelatedBodiesAndPinnedResponses(t *testing.T) {
	f := newAPIFixture(t)
	pulls := make([]PullInput, 100)
	for i := range pulls {
		pulls[i] = nativePull(int64(i + 1))
	}
	for i := 0; i < 3; i++ {
		run := nativeRun(fmt.Sprintf("imported-%d", i))
		run.Findings[0].Detail = strings.Repeat(fmt.Sprintf("Retained source %d. ", i), 512)
		pulls[i].Runs = []RunInput{run}
	}
	initial, _, _ := transferApplyFor(t, f, transferRequest(0, TransferGroup{Source: "test-import", Pulls: pulls}), "native-growth-import")
	first := initial.ID
	detailRoute := "/runs/" + runOpaqueID(f.project, "test-import", pulls[1].Runs[0].SourceRunID) + "?snapshot=" + url.QueryEscape(first)
	var detail RunDetailResponse
	originalDetail := nativeRead(t, f, detailRoute, &detail)
	var queue QueueResponse
	nativeRead(t, f, "/queue?limit=1", &queue)
	originalCursor := queue.Cursor
	if originalCursor == "" {
		t.Fatal("missing original continuation")
	}
	var originalPage QueueResponse
	pageRoute := "/queue?limit=1&cursor=" + url.QueryEscape(originalCursor)
	originalPageBytes := nativeRead(t, f, pageRoute, &originalPage)
	for _, batch := range []struct {
		source string
		first  int
	}{{"producer-a", 0}, {"producer-b", 0}, {"producer-a", 75}} {
		nativeGrowthTransferBatch(t, f, pulls[0], batch.source, batch.first, 75, 5)
	}
	// The captured native run is a new original producer, not imported credit.
	captured := nativeRun("native-after-retained-growth")
	mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &captured})
	newPR := nativeAttempt("native-pr-101-attempt", "prepared")
	mustNativePublish(t, f, NativePublication{Pull: nativePull(101), Attempt: &newPR})
	var beforeBodies, beforeDetailBytes, beforePages int64
	if err := f.store.DB().QueryRow(`SELECT count(*),sum(length(CAST(detail AS BLOB))) FROM review_finding_versions`).Scan(&beforeBodies, &beforeDetailBytes); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`PRAGMA page_count`).Scan(&beforePages); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		attempt := nativeAttempt(fmt.Sprintf("native-growth-%d", i), "prepared")
		mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Attempt: &attempt})
	}
	var afterBodies, afterDetailBytes, afterPages, runBodies, unrelatedPullBodies int64
	if err := f.store.DB().QueryRow(`SELECT count(*),sum(length(CAST(detail AS BLOB))) FROM review_finding_versions`).Scan(&afterBodies, &afterDetailBytes); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_run_versions`).Scan(&runBodies); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_pull_versions WHERE number IN(2,3)`).Scan(&unrelatedPullBodies); err != nil {
		t.Fatal(err)
	}
	if beforeBodies != 1129 || afterBodies != beforeBodies || afterDetailBytes != beforeDetailBytes || runBodies != 229 || unrelatedPullBodies != 2 {
		t.Fatalf("attempt-only publication copied unrelated payloads: findings%d→%d bytes%d→%d runs%d pulls%d", beforeBodies, afterBodies, beforeDetailBytes, afterDetailBytes, runBodies, unrelatedPullBodies)
	}
	if got := nativeRead(t, f, detailRoute, &detail); string(got) != string(originalDetail) {
		t.Fatal("retained detail changed after unrelated publications")
	}
	if got := nativeRead(t, f, pageRoute, &originalPage); string(got) != string(originalPageBytes) {
		t.Fatal("original signed continuation changed meaning")
	}
	var current PullResponse
	nativeRead(t, f, "/pulls/1", &current)
	if current.Pull.RunCount != 227 || current.Pull.AttemptCount != 285 || current.Pull.RecentRuns[0].Source != nativeSource {
		t.Fatal("mixed-source current summary lost imported evidence or attempts")
	}
	for source, want := range map[string]int{"producer-a": 150, "producer-b": 75, "test-import": 3, nativeSource: 1} {
		var got int
		if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_run_versions WHERE project_id=? AND source=?`, f.project, source).Scan(&got); err != nil || got != want {
			t.Fatal("original producer records changed", source, got, want, err)
		}
	}
	var pullCount int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_current_pulls WHERE project_id=?`, f.project).Scan(&pullCount); err != nil || pullCount != 101 {
		t.Fatal("native PR101 not appended", pullCount, err)
	}
	if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	if err := ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`PRAGMA page_count`).Scan(&afterPages); err != nil {
		t.Fatal(err)
	}
	t.Logf("60 actual native publications: retainedfindingbytes=%d, runbodies=%d, unrelatedpullbodies=%d, logicaldatabasepages=%d→%d; retained totals exceed old lifetime boundaries", afterDetailBytes, runBodies, unrelatedPullBodies, beforePages, afterPages)
}

// Seed only through the actual administrator preview/apply boundary. Every
// bounded batch preserves original source keys and installs canonical caches.
func nativeGrowthTransferBatch(t *testing.T, f *apiFixture, metadata PullInput, source string, first, count, findingCount int) Receipt {
	t.Helper()
	metadata.Complete = false
	metadata.Runs, metadata.Attempts = nil, nil
	for i := first; i < first+count; i++ {
		id := fmt.Sprintf("retained-%03d", i)
		run := RunInput{SourceRunID: id, Channel: "cli", HeadSHA: metadata.HeadSHA, Scope: "Retained selected history", Outcome: "unknown", Found: int64(findingCount)}
		for j := 0; j < findingCount; j++ {
			run.Findings = append(run.Findings, Finding{SourceFindingID: fmt.Sprintf("finding-%d", j), Title: "Original retained finding", Detail: "Original retained literal detail", Severity: "unknown", Disposition: "unadjudicated"})
		}
		metadata.Runs = append(metadata.Runs, run)
		metadata.Attempts = append(metadata.Attempts, Attempt{SourceAttemptID: id, SourceRunID: id, Channel: "cli", HeadSHA: metadata.HeadSHA, State: "unknown"})
	}
	var revision int64
	if err := f.store.DB().QueryRow(`SELECT COALESCE((SELECT revision FROM review_workspace WHERE project_id=?),0)`, f.project).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	receipt, _, _ := transferApplyFor(t, f, transferRequest(revision, TransferGroup{Source: source, Pulls: []PullInput{metadata}}), fmt.Sprintf("growth-%s-%d", source, first))
	return receipt
}
