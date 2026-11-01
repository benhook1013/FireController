package reviews

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Existing HTTP readers exercise the actual installed version-four schema.
func historyViewsFixture(t *testing.T) *apiFixture {
	t.Helper()
	return newAPIFixture(t)
}

func historyViewsStage(t *testing.T, tx *sql.Tx, w *historySnapshotWrite, spec historyRecordSpec, keys, body, placement []any, detail bool) int64 {
	t.Helper()
	version, err := w.version(t.Context(), tx, spec, keys, body, detail)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.membership(t.Context(), tx, spec, keys, version, placement); err != nil {
		t.Fatal(err)
	}
	return version
}

func historyViewsRun(project string, index int) (string, string, string) {
	source, external := "producer-a", fmt.Sprintf("run-%03d", index)
	if index < 2 {
		external = "shared-run"
		if index == 1 {
			source = "producer-b"
		}
	}
	return runOpaqueID(project, source, external), source, external
}
func historyViewsAttempt(index int) (string, string) {
	source, external := "producer-a", fmt.Sprintf("attempt-%03d", index)
	if index < 2 {
		external = "shared-attempt"
		if index == 1 {
			source = "producer-b"
		}
	}
	return source, external
}
func historyViewsPullBody(title string, count int) []any {
	return []any{title, "merged", strings.Repeat("a", 40), "main", strings.Repeat("b", 40), "unknown", "unknown", true, count, count, "[]", "[]"}
}
func historyViewsRunBody(source, external string, index int, fixed int) []any {
	found := 0
	if index == 0 {
		found = 1
		fixed = 1
	}
	return []any{123, source, external, "cli", strings.Repeat("a", 40), "Synthetic reviewer", "bounded fixture", "completed", true, true, "2026-10-01T00:00:00Z", "2026-10-01T00:01:00Z", found, found, 0, true, fixed, "[]"}
}
func historyViewsAttemptBody(index int, completed bool) []any {
	state, finished := "active", ""
	var duration any
	if completed {
		state = "completed"
		finished = "2026-10-01T00:01:00Z"
		duration = int64(60)
	}
	_, _, run := historyViewsRun("unused-project", index)
	return []any{123, "cli", strings.Repeat("a", 40), state, "2026-10-01T00:00:00Z", finished, duration, run, "", ""}
}
func historyViewsFinding(t *testing.T, tx *sql.Tx, project, id string, revision int64, proof string) {
	t.Helper()
	_, err := tx.ExecContext(t.Context(), `INSERT INTO review_finding_versions VALUES(?,?,?,123,0,'finding-one','Retained title','Private retained finding body','medium','accepted','Confirmed','Builder','2026-10-01T00:01:00Z',?,?,'2026-10-01T00:01:00Z','null')`, project, id, revision, strings.Repeat("c", 40), proof)
	if err != nil {
		t.Fatal(err)
	}
}
func historyViewsSeed(t *testing.T, f *apiFixture, count int) string {
	t.Helper()
	tx := historyWriterTx(t, f)
	snapshot := historyWriterSnapshot(t, tx, f.project, 1)
	w := newHistorySnapshotWrite(f.project, 1)
	historyViewsStage(t, tx, w, historyPullRecord, []any{123}, historyViewsPullBody("Initial retained PR", count), []any{0}, false)
	historyViewsStage(t, tx, w, historyPullRecord, []any{124}, historyViewsPullBody("Other retained merged PR", 0), []any{1}, false)
	for i := 0; i < count; i++ {
		id, source, external := historyViewsRun(f.project, i)
		version := historyViewsStage(t, tx, w, historyRunRecord, []any{id}, historyViewsRunBody(source, external, i, 0), []any{123, i}, false)
		if i == 0 {
			historyViewsFinding(t, tx, f.project, id, version, "Original proof")
		}
		source, external = historyViewsAttempt(i)
		historyViewsStage(t, tx, w, historyAttemptRecord, []any{source, external}, historyViewsAttemptBody(i, i != 0), []any{123, i}, false)
	}
	historyViewsStage(t, tx, w, historyRetiredRecord, []any{125}, []any{"superseded"}, []any{0}, false)
	if err := w.applyCurrent(t.Context(), tx, snapshot, 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// Hash physical retained state, authority, receipts and events around HTTP reads.
// No writable handler is used after installing read-only compatibility views.
func historyViewsDigest(t *testing.T, f *apiFixture) [32]byte {
	t.Helper()
	h := sha256.New()
	tables := []string{"review_snapshots", "review_workspace", "review_finding_versions", "credentials", "grants", "receipts", "events", "metadata"}
	for _, spec := range []historyRecordSpec{historyPullRecord, historyRunRecord, historyAttemptRecord, historyRetiredRecord} {
		tables = append(tables, spec.versions, spec.memberships, spec.current)
	}
	for _, table := range tables {
		rows, err := f.store.DB().QueryContext(t.Context(), "SELECT * FROM "+table+" ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		h.Write([]byte(table))
		for rows.Next() {
			values, targets := make([]any, len(columns)), make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err := rows.Scan(targets...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			encoded, err := json.Marshal(values)
			if err != nil {
				rows.Close()
				t.Fatal(err)
			}
			h.Write(encoded)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func TestHistoryViewsHTTPArchivesAndPinnedPreflight(t *testing.T) {
	const count = 123
	f := historyViewsFixture(t)
	first := historyViewsSeed(t, f, count)
	beforeReads := historyViewsDigest(t, f)
	var queue QueueResponse
	queueBytes := nativeRead(t, f, "/queue?limit=1", &queue)
	if len(queue.Items) != 1 || queue.Items[0].Number != 123 || queue.Items[0].State != "merged" || queue.Items[0].RunCount != count || queue.Items[0].AttemptCount != count || !queue.HasMore || queue.Cursor == "" || strings.Contains(string(queueBytes), "Private retained finding body") {
		t.Fatalf("queue compatibility: %+v", queue)
	}
	var pull PullResponse
	pullBytes := nativeRead(t, f, "/pulls/123", &pull)
	if pull.Pull.RunCount != count || strings.Contains(string(pullBytes), `"attempts":`) || strings.Contains(string(pullBytes), "Private retained finding body") {
		t.Fatal("PR ceased being compact", string(pullBytes))
	}
	var runs RunsResponse
	nativeRead(t, f, "/pulls/123/runs?limit=5", &runs)
	var attempts AttemptsResponse
	nativeRead(t, f, "/pulls/123/attempts?limit=5", &attempts)
	if len(runs.Items) != 5 || len(attempts.Items) != 5 || !runs.HasMore || !attempts.HasMore {
		t.Fatal("archive page bound lost")
	}
	runCursor, attemptCursor := runs.Cursor, attempts.Cursor
	id, source, external := historyViewsRun(f.project, 0)
	var oldDetail RunDetailResponse
	oldDetailBytes := nativeRead(t, f, "/runs/"+id+"?snapshot="+first, &oldDetail)
	if oldDetail.Source != "writer-fixture" || oldDetail.Run.Source != source || oldDetail.Run.SourceRunID != external || len(oldDetail.Findings) != 1 || oldDetail.Findings[0].ProofNote != "Original proof" {
		t.Fatal("publisher/producer/detail boundary lost", oldDetail)
	}
	if historyViewsDigest(t, f) != beforeReads {
		t.Fatal("GET reads changed retained state")
	}

	tx := historyWriterTx(t, f)
	second := historyWriterSnapshot(t, tx, f.project, 2)
	next := newHistorySnapshotWrite(f.project, 2)
	historyViewsStage(t, tx, next, historyPullRecord, []any{123}, historyViewsPullBody("Updated current PR", 1), []any{0}, false)
	version := historyViewsStage(t, tx, next, historyRunRecord, []any{id}, historyViewsRunBody(source, external, 0, 1), []any{123, 0}, true)
	historyViewsFinding(t, tx, f.project, id, version, "New reviewed proof")
	source, external = historyViewsAttempt(0)
	historyViewsStage(t, tx, next, historyAttemptRecord, []any{source, external}, historyViewsAttemptBody(0, true), []any{123, 0}, false)
	historyViewsStage(t, tx, next, historyRetiredRecord, []any{125}, []any{"defunct"}, []any{0}, false)
	for i := 1; i < count; i++ {
		run, _, _ := historyViewsRun(f.project, i)
		if err := next.remove(t.Context(), tx, historyRunRecord, []any{run}); err != nil {
			t.Fatal(err)
		}
		source, external := historyViewsAttempt(i)
		if err := next.remove(t.Context(), tx, historyAttemptRecord, []any{source, external}); err != nil {
			t.Fatal(err)
		}
	}
	// Real logical views must still use old exact current references before the
	// pointer advance, even though intervals now describe the prospective view.
	for _, test := range []struct {
		snapshot, title, proof, state, retired string
		wantCount                              int
	}{{first, "Initial retained PR", "Original proof", "active", "superseded", count}, {second, "Updated current PR", "New reviewed proof", "completed", "defunct", 1}} {
		var title, proof, state, retired string
		var total int
		if err := tx.QueryRowContext(t.Context(), "SELECT title FROM review_pulls WHERE project_id=? AND snapshot_id=? AND number=123", f.project, test.snapshot).Scan(&title); err != nil || title != test.title {
			t.Fatal("preflight pull", title, err)
		}
		if err := tx.QueryRowContext(t.Context(), "SELECT proof_note FROM review_findings WHERE project_id=? AND snapshot_id=? AND run_id=?", f.project, test.snapshot, id).Scan(&proof); err != nil || proof != test.proof {
			t.Fatal("preflight exact finding version", proof, err)
		}
		if err := tx.QueryRowContext(t.Context(), "SELECT state FROM review_attempts WHERE project_id=? AND snapshot_id=? AND source='producer-a' AND source_attempt_id='shared-attempt'", f.project, test.snapshot).Scan(&state); err != nil || state != test.state {
			t.Fatal("preflight attempt", state, err)
		}
		if err := tx.QueryRowContext(t.Context(), "SELECT reason FROM review_retired_pulls WHERE project_id=? AND snapshot_id=? AND number=125", f.project, test.snapshot).Scan(&retired); err != nil || retired != test.retired {
			t.Fatal("preflight retired", retired, err)
		}
		if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM review_runs WHERE project_id=? AND snapshot_id=? AND pull_number=123", f.project, test.snapshot).Scan(&total); err != nil || total != test.wantCount {
			t.Fatal("preflight archive", total, err)
		}
	}
	if err := next.applyCurrent(t.Context(), tx, second, 1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	beforeReads = historyViewsDigest(t, f)
	var current PullResponse
	nativeRead(t, f, "/pulls/123", &current)
	if current.Revision != 2 || current.Pull.Title != "Updated current PR" || current.Pull.RunCount != 1 || current.Pull.AttemptCount != 1 {
		t.Fatal("new current did not advance", current)
	}
	var currentRuns RunsResponse
	nativeRead(t, f, "/pulls/123/runs", &currentRuns)
	if len(currentRuns.Items) != 1 || currentRuns.HasMore {
		t.Fatal("omitted current runs reappeared")
	}
	var currentAttempts AttemptsResponse
	nativeRead(t, f, "/pulls/123/attempts", &currentAttempts)
	if len(currentAttempts.Items) != 1 || currentAttempts.Items[0].State != "completed" || currentAttempts.Items[0].DurationSeconds == nil || *currentAttempts.Items[0].DurationSeconds != 60 {
		t.Fatal("current attempt version lost", currentAttempts)
	}
	var newDetail RunDetailResponse
	nativeRead(t, f, "/runs/"+id, &newDetail)
	if len(newDetail.Findings) != 1 || newDetail.Findings[0].ProofNote != "New reviewed proof" || newDetail.Run.Fixed != 1 {
		t.Fatal("new finding joined old version")
	}
	var pinnedDetail RunDetailResponse
	if got := nativeRead(t, f, "/runs/"+id+"?snapshot="+first, &pinnedDetail); string(got) != string(oldDetailBytes) {
		t.Fatal("old detail bytes changed")
	}
	var oldTail QueueResponse
	nativeRead(t, f, "/queue?limit=1&cursor="+url.QueryEscape(queue.Cursor), &oldTail)
	if oldTail.SnapshotID != first || oldTail.Revision != 1 || len(oldTail.Items) != 1 || oldTail.Items[0].Number != 124 || oldTail.HasMore {
		t.Fatal("old queue continuation drifted", oldTail)
	}
	seenRuns := map[string]bool{}
	seenAttempts := map[string]bool{}
	for _, r := range runs.Items {
		seenRuns[r.ID] = true
	}
	for _, a := range attempts.Items {
		seenAttempts[a.Source+"\x00"+a.SourceAttemptID] = true
	}
	for pageNumber := 1; runCursor != ""; pageNumber++ {
		if pageNumber > count {
			t.Fatal("run continuation did not terminate")
		}
		var page RunsResponse
		nativeRead(t, f, "/pulls/123/runs?limit=5&cursor="+url.QueryEscape(runCursor), &page)
		if page.Revision != 1 || page.SnapshotID != first || len(page.Items) > 5 {
			t.Fatal("run cursor drift/page bound")
		}
		for _, r := range page.Items {
			if seenRuns[r.ID] {
				t.Fatal("duplicate run page")
			}
			seenRuns[r.ID] = true
		}
		if page.HasMore && page.Cursor == "" {
			t.Fatal("missing run continuation")
		}
		runCursor = page.Cursor
	}
	for pageNumber := 1; attemptCursor != ""; pageNumber++ {
		if pageNumber > count {
			t.Fatal("attempt continuation did not terminate")
		}
		var page AttemptsResponse
		nativeRead(t, f, "/pulls/123/attempts?limit=5&cursor="+url.QueryEscape(attemptCursor), &page)
		if page.Revision != 1 || page.SnapshotID != first || len(page.Items) > 5 {
			t.Fatal("attempt cursor drift/page bound")
		}
		for _, a := range page.Items {
			key := a.Source + "\x00" + a.SourceAttemptID
			if seenAttempts[key] {
				t.Fatal("duplicate attempt page")
			}
			seenAttempts[key] = true
			if a.SourceAttemptID == "shared-attempt" && a.Source == "producer-a" && (a.State != "active" || a.DurationSeconds != nil) {
				t.Fatal("old attempt changed through latest body join")
			}
		}
		if page.HasMore && page.Cursor == "" {
			t.Fatal("missing attempt continuation")
		}
		attemptCursor = page.Cursor
	}
	a, _, _ := historyViewsRun(f.project, 0)
	b, _, _ := historyViewsRun(f.project, 1)
	if len(seenRuns) != count || len(seenAttempts) != count || !seenRuns[a] || !seenRuns[b] || a == b || !seenAttempts["producer-a\x00shared-attempt"] || !seenAttempts["producer-b\x00shared-attempt"] {
		t.Fatal("original producers collapsed or archive truncated")
	}
	for table, want := range map[string]int{"review_run_versions": 124, "review_finding_versions": 2, "review_attempt_versions": 124, "review_pull_versions": 3} {
		var n int
		if err := f.store.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != want {
			t.Fatal("unrelated payload bodies copied", table, n, want, err)
		}
	}
	if historyViewsDigest(t, f) != beforeReads {
		t.Fatal("archive HTTP GETs changed retained state")
	}
}

func TestHistoryViewsHTTPProjectCursorAndReadGuards(t *testing.T) {
	f := historyViewsFixture(t)
	first := historyViewsSeed(t, f, 7)
	base := "/api/v1/projects/" + f.project + "/reviews"
	var queue QueueResponse
	nativeRead(t, f, "/queue?limit=1", &queue)
	var runs RunsResponse
	started := time.Now()
	nativeRead(t, f, "/pulls/123/runs?limit=5", &runs)
	t.Logf("actual current-view HTTP run page: returned=%d elapsed=%s (observation only)", len(runs.Items), time.Since(started))
	var attempts AttemptsResponse
	nativeRead(t, f, "/pulls/123/attempts?limit=5", &attempts)
	tx := historyWriterTx(t, f)
	if _, err := tx.ExecContext(t.Context(), `INSERT INTO projects(id,name,github_repository_id,repository) VALUES('history-foreign','Foreign',702,'owner/foreign')`); err != nil {
		t.Fatal(err)
	}
	foreignSnapshot := historyWriterSnapshot(t, tx, "history-foreign", 1)
	if _, err := tx.ExecContext(t.Context(), `UPDATE review_snapshots SET snapshot_id='foreign-snapshot' WHERE project_id='history-foreign' AND snapshot_id=?`, foreignSnapshot); err != nil {
		t.Fatal(err)
	}
	foreignSnapshot = "foreign-snapshot"
	foreign := newHistorySnapshotWrite("history-foreign", 1)
	historyViewsStage(t, tx, foreign, historyPullRecord, []any{123}, historyViewsPullBody("PRIVATE FOREIGN SENTINEL", 1), []any{0}, false)
	foreignID, source, external := historyViewsRun("history-foreign", 0)
	version := historyViewsStage(t, tx, foreign, historyRunRecord, []any{foreignID}, historyViewsRunBody(source, external, 0, 0), []any{123, 0}, false)
	historyViewsFinding(t, tx, "history-foreign", foreignID, version, "Foreign proof sentinel")
	source, external = historyViewsAttempt(0)
	historyViewsStage(t, tx, foreign, historyAttemptRecord, []any{source, external}, historyViewsAttemptBody(0, false), []any{123, 0}, false)
	if err := foreign.applyCurrent(t.Context(), tx, foreignSnapshot, 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	before := historyViewsDigest(t, f)
	for _, test := range []struct {
		name, path, token string
		status            int
	}{
		{"no credential", base + "/queue?cursor=" + url.QueryEscape(queue.Cursor), "", 401},
		{"foreign project", "/api/v1/projects/history-foreign/reviews/queue", f.readToken, 403},
		{"foreign snapshot", base + "/pulls/123?snapshot=" + foreignSnapshot, f.readToken, 404},
		{"foreign run", base + "/runs/" + foreignID, f.readToken, 404},
		{"foreign cursor", "/api/v1/projects/history-foreign/reviews/queue?cursor=" + url.QueryEscape(queue.Cursor), f.admin, 400},
		{"different PR", base + "/pulls/124/runs?cursor=" + url.QueryEscape(runs.Cursor), f.readToken, 400},
		{"different archive", base + "/pulls/123/attempts?cursor=" + url.QueryEscape(runs.Cursor), f.readToken, 400},
		{"snapshot mismatch", base + "/pulls/123/attempts?snapshot=not-the-cursor-snapshot&cursor=" + url.QueryEscape(attempts.Cursor), f.readToken, 400},
		{"tampered cursor", base + "/queue?cursor=" + url.QueryEscape(queue.Cursor+"x"), f.readToken, 400},
		{"oversize page", base + "/pulls/123/runs?limit=101", f.readToken, 400},
		{"duplicate page bound", base + "/pulls/123/attempts?limit=1&limit=2", f.readToken, 400},
	} {
		response := f.call(http.MethodGet, test.path, test.token, "", nil)
		if response.Code != test.status || strings.Contains(response.Body.String(), "PRIVATE FOREIGN SENTINEL") || strings.Contains(response.Body.String(), "Foreign proof sentinel") {
			t.Fatalf("%s: status=%d expected=%d body=%s", test.name, response.Code, test.status, response.Body.String())
		}
	}
	var own PullResponse
	body := nativeRead(t, f, "/pulls/123?snapshot="+first, &own)
	if own.Pull.Title != "Initial retained PR" || strings.Contains(string(body), "FOREIGN") {
		t.Fatal("same external source keys leaked another project")
	}
	if before != historyViewsDigest(t, f) {
		t.Fatal("authorized/refused GETs changed application state")
	}
	// Inspect the actual view query, not just the underlying membership table.
	// SQLite can retain a historical UNION branch/temp sort in this plan; this
	// observation is not a claim of constant historical query CPU.
	rows, err := f.store.DB().QueryContext(t.Context(), `EXPLAIN QUERY PLAN SELECT id FROM review_runs WHERE project_id=? AND snapshot_id=? AND pull_number=123 AND run_order<120 ORDER BY run_order DESC LIMIT 6`, f.project, first)
	if err != nil {
		t.Fatal(err)
	}
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("actual current-view page query plan: %s", strings.Join(details, " | "))
	if _, err := f.store.DB().ExecContext(t.Context(), `UPDATE credentials SET revoked=1 WHERE id='review-read-credential'`); err != nil {
		t.Fatal(err)
	}
	if response := f.call(http.MethodGet, base+"/pulls/123/runs?cursor="+url.QueryEscape(runs.Cursor), f.readToken, "", nil); response.Code != 401 {
		t.Fatal("old signed continuation bypassed revocation", response.Code)
	}
}
