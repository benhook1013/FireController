package reviews

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

// This fixture deliberately bypasses observation admission: it proves bounded
// archive reads beyond the old count ceiling, not cap-free publication.
func seedAttemptArchive(t *testing.T, f *apiFixture, count int) string {
	t.Helper()
	tx := historyWriterTx(t, f)
	snapshot := core.NewID("review_snapshot")
	observed := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(t.Context(), `INSERT INTO review_snapshots(project_id,snapshot_id,revision,repository_id,source,observed_at,recorded_at,payload_sha256) VALUES(?,?,1,701,'test-import',?,?,?)`, f.project, snapshot, observed, observed, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	write := newHistorySnapshotWrite(f.project, 1)
	for position := 0; position < count; position++ {
		source, id := "producer-a", fmt.Sprintf("attempt-%03d", position)
		if position < 2 {
			id = "shared-attempt"
			if position == 1 {
				source = "producer-b"
			}
		}
		if err := write.attempt(t.Context(), tx, source, 1, position, Attempt{SourceAttemptID: id, Channel: "cli", State: "unknown"}); err != nil {
			t.Fatal(err)
		}
	}
	latest := []AttemptSummary{}
	if count > 0 {
		id := fmt.Sprintf("attempt-%03d", count-1)
		if count <= 2 {
			id = "shared-attempt"
		}
		latest = append(latest, AttemptSummary{SourceAttemptID: id, Channel: "cli", State: "unknown"})
	}
	for position, pull := range []PullInput{samplePull(1, "Retained"), samplePull(2, "Other")} {
		attempts, compact := 0, "[]"
		if pull.Number == 1 {
			attempts = count
			compact = mustJSON(latest)
		}
		body := []any{pull.Title, pull.State, pull.HeadSHA, pull.BaseRef, pull.BaseSHA, pull.CIState, pull.Mergeability, pull.Complete, 0, attempts, "[]", compact, "{}", int64(1)}
		if err := write.pull(t.Context(), tx, pull.Number, position, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := write.applyCurrent(t.Context(), tx, snapshot, 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func attemptArchivePage(t *testing.T, f *apiFixture, path string) AttemptsResponse {
	t.Helper()
	w := f.call("GET", "/api/v1/projects/"+f.project+"/reviews"+path, f.readToken, "", nil)
	var page AttemptsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &page); w.Code != 200 || err != nil {
		t.Fatalf("attempt page: %d %s (%v)", w.Code, w.Body.String(), err)
	}
	return page
}

func TestAttemptArchiveBoundedStablePagesAndOriginalProducers(t *testing.T) {
	f := newAPIFixture(t)
	snapshot := seedAttemptArchive(t, f, 123)
	before := nativeSnapshotBytes(t, f, snapshot)
	events, receipts := countRows(t, f.store, "events"), countRows(t, f.store, "receipts")
	page := attemptArchivePage(t, f, "/pulls/1/attempts")
	if len(page.Items) != 20 || !page.HasMore || page.Cursor == "" || page.Items[0].SourceAttemptID != "attempt-122" || page.Items[19].SourceAttemptID != "attempt-103" || page.Source != "test-import" || page.Items[0].Source != "producer-a" {
		t.Fatalf("default page is not bounded/newest-first: %+v", page)
	}
	var pull PullResponse
	raw := nativeRead(t, f, "/pulls/1", &pull)
	if pull.Pull.AttemptCount != 123 || strings.Contains(string(raw), `"attempts":`) || len(raw) > 4096 {
		t.Fatal("PR summary expanded the archive")
	}
	if nativeSnapshotBytes(t, f, snapshot) != before || countRows(t, f.store, "events") != events || countRows(t, f.store, "receipts") != receipts {
		t.Fatal("archive reads changed durable state")
	}
	// A later complete observation no longer exposes these attempts as current.
	// The signed continuation must still finish the original immutable archive.
	f.observe(t, reviewRequest(1, []PullInput{samplePull(1, "New metadata"), samplePull(2, "Other")}), "archive-after")
	current := attemptArchivePage(t, f, "/pulls/1/attempts")
	if len(current.Items) != 0 || current.HasMore || current.Cursor != "" || current.SnapshotID == snapshot {
		t.Fatalf("empty current page malformed: %+v", current)
	}
	seen := map[string]bool{}
	for {
		if page.SnapshotID != snapshot || page.Revision != 1 || len(page.Items) > 20 || len(page.Items) == 0 {
			t.Fatal("continuation changed snapshot or exceeded requested bound")
		}
		for _, item := range page.Items {
			key := item.Source + "/" + item.SourceAttemptID
			if seen[key] {
				t.Fatal("duplicate archive identity", key)
			}
			seen[key] = true
		}
		if !page.HasMore {
			if page.Cursor != "" {
				t.Fatal("complete page retained a continuation")
			}
			break
		}
		page = attemptArchivePage(t, f, "/pulls/1/attempts?limit=20&cursor="+url.QueryEscape(page.Cursor))
	}
	if len(seen) != 123 || !seen["producer-a/shared-attempt"] || !seen["producer-b/shared-attempt"] || nativeSnapshotBytes(t, f, snapshot) != before {
		t.Fatal("lost source-qualified identities or changed historical bytes")
	}
}

func TestAttemptArchiveRejectsCrossScopeAndMalformedQueries(t *testing.T) {
	f := newAPIFixture(t)
	_ = seedAttemptArchive(t, f, 3)
	page := attemptArchivePage(t, f, "/pulls/1/attempts?limit=1")
	cursor := url.QueryEscape(page.Cursor)
	base := "/api/v1/projects/" + f.project + "/reviews"
	for _, path := range []string{
		"/pulls/2/attempts?cursor=" + cursor,
		"/pulls/1/runs?cursor=" + cursor,
		"/queue?cursor=" + cursor,
		"/pulls/1/attempts?snapshot=review_snapshot_" + strings.Repeat("a", 32) + "&cursor=" + cursor,
		"/pulls/01/attempts", "/pulls/1/attempts?limit=0", "/pulls/1/attempts?limit=101",
		"/pulls/1/attempts?limit=1&limit=2", "/pulls/1/attempts?unexpected=1", "/pulls/1/attempts?cursor=broken",
	} {
		if w := f.call("GET", base+path, f.readToken, "", nil); w.Code != 400 {
			t.Fatalf("invalid scope/query admitted: %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	// The bootstrap administrator has another principal, even on this project.
	if w := f.call("GET", base+"/pulls/1/attempts?cursor="+cursor, f.admin, "", nil); w.Code != 400 {
		t.Fatalf("cross-principal cursor admitted: %d %s", w.Code, w.Body.String())
	}
	otherInstance := newAPIFixture(t)
	if w := otherInstance.call("GET", base+"/pulls/1/attempts?cursor="+cursor, otherInstance.readToken, "", nil); w.Code != 400 {
		t.Fatalf("cross-instance cursor admitted: %d %s", w.Code, w.Body.String())
	}
	if _, err := f.store.DB().Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES('other-archive-project','Other',702,'owner/other-archive')`); err != nil {
		t.Fatal(err)
	}
	if w := f.call("GET", "/api/v1/projects/other-archive-project/reviews/pulls/1/attempts?cursor="+cursor, f.admin, "", nil); w.Code != 400 {
		t.Fatalf("cross-project cursor admitted: %d %s", w.Code, w.Body.String())
	}
	if w := f.call("GET", base+"/pulls/1/attempts", "", "", nil); w.Code != 401 {
		t.Fatalf("unauthenticated archive read: %d", w.Code)
	}
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='review-read-credential'`); err != nil {
		t.Fatal(err)
	}
	if w := f.call("GET", base+"/pulls/1/attempts?cursor="+cursor, f.readToken, "", nil); w.Code != 401 {
		t.Fatalf("revoked continuation admitted: %d", w.Code)
	}
}
