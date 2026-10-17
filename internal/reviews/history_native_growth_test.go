package reviews

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestNativeAttemptGrowthRetainsUnrelatedBodiesAndPinnedResponses(t *testing.T) {
	f := newAPIFixture(t)
	pulls := []PullInput{nativePull(1), nativePull(2), nativePull(3)}
	for i := range pulls {
		run := nativeRun(fmt.Sprintf("imported-%d", i))
		run.Findings[0].Detail = strings.Repeat(fmt.Sprintf("Retained source %d. ", i), 512)
		pulls[i].Runs = []RunInput{run}
	}
	first := f.observe(t, reviewRequest(0, pulls), "native-growth-import")["id"].(string)
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
	if beforeBodies != 3 || afterBodies != beforeBodies || afterDetailBytes != beforeDetailBytes || runBodies != 3 || unrelatedPullBodies != 2 {
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
	if current.Pull.RunCount != 1 || current.Pull.AttemptCount != 60 || current.Pull.RecentRuns[0].Source != "test-import" {
		t.Fatal("mixed-source current summary lost imported evidence or attempts")
	}
	if err := ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`PRAGMA page_count`).Scan(&afterPages); err != nil {
		t.Fatal(err)
	}
	t.Logf("60 actual native publications: retainedfindingbytes=%d, runbodies=%d, unrelatedpullbodies=%d, logicaldatabasepages=%d→%d; publication caps remain enabled", afterDetailBytes, runBodies, unrelatedPullBodies, beforePages, afterPages)
}
