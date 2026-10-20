package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func queueEditRequest(revision int64, queue []QueuePullInput, retired []RetiredPull) QueueEditRequest {
	if queue == nil {
		queue = []QueuePullInput{}
	}
	if retired == nil {
		retired = []RetiredPull{}
	}
	return QueueEditRequest{ExpectedRevision: &revision, RepositoryID: 701, Source: "operator-queue-edit", Queue: queue, Retired: retired}
}

func queueEditHTTP(f *apiFixture, req QueueEditRequest, key string) *httptest.ResponseRecorder {
	return f.call(http.MethodPost, importBase(f)+"/queue/edits", f.admin, key, req)
}

func TestQueueEditsBoundedSelectionPreservesHistoryAndReactivates(t *testing.T) {
	f := newAPIFixture(t)
	// Add more than 201 pulls through ordinary bounded additive batches. This
	// proves queue edits do not require a full-list replacement to change one
	// entry or retire another.
	for batch := 0; batch < 3; batch++ {
		start, end := batch*100+1, (batch+1)*100
		if end > 202 {
			end = 202
		}
		group := TransferGroup{Source: "growth-producer"}
		for number := start; number <= end; number++ {
			pull := transferAdmissionPull(int64(number))
			pull.State = "open"
			if number == 202 {
				pull.State = "merged"
				pull.Runs = []RunInput{{SourceRunID: "retained-run", Channel: "cli", Scope: "full", Outcome: "unknown"}}
				pull.Attempts = []Attempt{{SourceAttemptID: "retained-attempt", Channel: "cli", State: "unknown"}}
			}
			group.Pulls = append(group.Pulls, pull)
		}
		groups := []TransferGroup{group}
		if batch == 2 {
			groups = append(groups, TransferGroup{Source: "second-producer", Pulls: []PullInput{{Number: 202, Title: "Selected pull 202", State: "merged", BaseRef: "main", CIState: "success", Mergeability: "mergeable", Runs: []RunInput{{SourceRunID: "second-run", Channel: "hosted", Scope: "full", Outcome: "unknown"}}, Attempts: []Attempt{{SourceAttemptID: "second-attempt", Channel: "hosted", State: "unknown"}}}}})
		}
		transferApplyFor(t, f, transferRequest(int64(batch), groups...), fmt.Sprintf("queue-growth-%d", batch))
	}
	base := importBase(f)
	oldFirst := f.call("GET", base+"/queue?limit=1", f.readToken, "", nil)
	var pinned QueueResponse
	if oldFirst.Code != 200 || json.Unmarshal(oldFirst.Body.Bytes(), &pinned) != nil || pinned.Items[0].Number != 1 || pinned.Cursor == "" {
		t.Fatalf("could not pin prior queue cursor: %d %s", oldFirst.Code, oldFirst.Body.String())
	}
	oldContinuationBefore := f.call("GET", base+"/queue?limit=1&cursor="+pinned.Cursor, f.readToken, "", nil)
	if oldContinuationBefore.Code != 200 {
		t.Fatalf("could not capture prior queue continuation: %d %s", oldContinuationBefore.Code, oldContinuationBefore.Body.String())
	}
	oldPull := f.call("GET", base+"/pulls/202?snapshot="+pinned.SnapshotID, f.readToken, "", nil)
	if oldPull.Code != 200 {
		t.Fatalf("prior detail unavailable: %d %s", oldPull.Code, oldPull.Body.String())
	}
	var previousObserved string
	if err := f.store.DB().QueryRow(`SELECT observed_at FROM review_snapshots WHERE project_id=? AND snapshot_id=?`, f.project, pinned.SnapshotID).Scan(&previousObserved); err != nil {
		t.Fatal(err)
	}

	beforePull := f.call("GET", base+"/pulls/2", f.readToken, "", nil)
	var omitted PullResponse
	if beforePull.Code != 200 || json.Unmarshal(beforePull.Body.Bytes(), &omitted) != nil {
		t.Fatalf("omitted pull unavailable: %d %s", beforePull.Code, beforePull.Body.String())
	}
	selected := QueuePullInput{Number: 202, Title: "Merged but retained", State: "merged", HeadSHA: strings.Repeat("a", 40), BaseRef: "main", CIState: "success", Mergeability: "mergeable"}
	newPull := QueuePullInput{Number: 203, Title: "New partial entry", State: "open", BaseRef: "main", CIState: "pending", Mergeability: "unknown"}
	edit := queueEditRequest(3, []QueuePullInput{selected, newPull}, []RetiredPull{{Number: 1, Reason: "superseded"}})
	first := queueEditHTTP(f, edit, "queue-edit-first")
	var receipt Receipt
	if first.Code != http.StatusCreated || json.Unmarshal(first.Body.Bytes(), &receipt) != nil || receipt.Revision != 4 || receipt.Outcome != "queue_edited" {
		t.Fatalf("selected queue edit: %d %s", first.Code, first.Body.String())
	}
	if got := queueEditHTTP(f, edit, "queue-edit-first"); got.Code != 201 || got.Body.String() != first.Body.String() {
		t.Fatalf("queue edit replay changed receipt: %d %s", got.Code, got.Body.String())
	}
	var latestObserved string
	if err := f.store.DB().QueryRow(`SELECT s.observed_at FROM review_workspace w JOIN review_snapshots s ON s.project_id=w.project_id AND s.snapshot_id=w.snapshot_id WHERE w.project_id=?`, f.project).Scan(&latestObserved); err != nil || latestObserved != previousObserved {
		t.Fatalf("operator edit invented observation freshness: %q %q %v", latestObserved, previousObserved, err)
	}
	page := f.call("GET", base+"/queue?limit=100", f.readToken, "", nil)
	var firstPage QueueResponse
	if page.Code != 200 || json.Unmarshal(page.Body.Bytes(), &firstPage) != nil || firstPage.Revision != 4 || len(firstPage.Items) != 100 {
		t.Fatalf("edited queue page: %d %s", page.Code, page.Body.String())
	}
	if firstPage.Items[0].Number != 2 || firstPage.Items[0].Title != omitted.Pull.Title || !firstPage.HasMore {
		t.Fatalf("retirement did not hide selected PR or shifted omitted PR: %+v", firstPage)
	}
	var added PullResponse
	addedResp := f.call("GET", base+"/pulls/203", f.readToken, "", nil)
	if addedResp.Code != 200 || json.Unmarshal(addedResp.Body.Bytes(), &added) != nil || added.Pull.Complete || added.Pull.RunCount != 0 || added.Pull.AttemptCount != 0 {
		t.Fatalf("new PR claimed unsupported coverage: %d %s", addedResp.Code, addedResp.Body.String())
	}
	var changed PullResponse
	changedResp := f.call("GET", base+"/pulls/202", f.readToken, "", nil)
	if changedResp.Code != 200 || json.Unmarshal(changedResp.Body.Bytes(), &changed) != nil || changed.Pull.Title != selected.Title || changed.Pull.Complete || changed.Pull.RunCount != 2 || changed.Pull.AttemptCount != 2 || changed.Pull.State != "merged" {
		t.Fatalf("metadata edit dropped history or merged state: %d %s", changedResp.Code, changedResp.Body.String())
	}
	retiredDetail := f.call("GET", base+"/pulls/1", f.readToken, "", nil)
	if retiredDetail.Code != 200 {
		t.Fatalf("retirement hid retained detail: %d %s", retiredDetail.Code, retiredDetail.Body.String())
	}
	oldContinuation := f.call("GET", base+"/queue?limit=1&cursor="+pinned.Cursor, f.readToken, "", nil)
	var oldPage QueueResponse
	if oldContinuation.Code != 200 || json.Unmarshal(oldContinuation.Body.Bytes(), &oldPage) != nil || oldPage.SnapshotID != pinned.SnapshotID || oldPage.Revision != pinned.Revision || len(oldPage.Items) != 1 || oldPage.Items[0].Number != 2 {
		t.Fatalf("old cursor changed after edit: %d %s", oldContinuation.Code, oldContinuation.Body.String())
	}
	oldPullAgain := f.call("GET", base+"/pulls/202?snapshot="+pinned.SnapshotID, f.readToken, "", nil)
	if oldContinuation.Code != 200 || oldContinuation.Body.String() != oldContinuationBefore.Body.String() {
		t.Fatal("queue editing changed byte-stable prior queue cursor response")
	}
	if oldPullAgain.Code != 200 || oldPullAgain.Body.String() != oldPull.Body.String() {
		t.Fatalf("queue editing changed prior detail bytes: before=%s after=%s", oldPull.Body.String(), oldPullAgain.Body.String())
	}
	visible := make(map[int64]PullSummary, 202)
	currentPage := firstPage
	for {
		for _, item := range currentPage.Items {
			visible[item.Number] = item
		}
		if !currentPage.HasMore {
			break
		}
		if currentPage.Cursor == "" {
			t.Fatal("current queue page omitted continuation cursor")
		}
		response := f.call("GET", base+"/queue?limit=100&cursor="+currentPage.Cursor, f.readToken, "", nil)
		currentPage = QueueResponse{}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &currentPage) != nil {
			t.Fatalf("current queue continuation failed: %d %s", response.Code, response.Body.String())
		}
	}
	if len(visible) != 202 || visible[202].State != "merged" || visible[203].Complete {
		t.Fatalf("queue edit removed merged PR, failed to preserve partial entry, or changed omission coverage: visible=%d merged=%+v new=%+v", len(visible), visible[202], visible[203])
	}
	if err := ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal("history references after queue edit:", err)
	}
	if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil {
		t.Fatal("current summaries after queue edit:", err)
	}
	// Native history can continue on a hidden PR without changing queue
	// visibility or clearing its explicit retirement marker.
	scope := transferTestScope(t, f)
	native := NativePublication{
		Pull:    PullInput{Number: 1, Title: "Selected pull 1", State: "open", BaseRef: "main", CIState: "success", Mergeability: "mergeable"},
		Attempt: &Attempt{SourceAttemptID: "native-hidden-attempt", Channel: "cli", State: "unknown"},
	}
	if _, err := f.store.Mutate(t.Context(), scope, core.Mutation{Operation: "test.native.hidden", Target: "workspace", Key: "native-hidden", Request: native}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		return PublishNative(ctx, tx, scope, native)
	}); err != nil {
		t.Fatal("native update on hidden PR:", err)
	}
	queueAfterNative := f.call("GET", base+"/queue?limit=100", f.readToken, "", nil)
	var afterNative QueueResponse
	if queueAfterNative.Code != 200 || json.Unmarshal(queueAfterNative.Body.Bytes(), &afterNative) != nil {
		t.Fatalf("queue read after native update failed: %d %s", queueAfterNative.Code, queueAfterNative.Body.String())
	}
	for _, item := range afterNative.Items {
		if item.Number == 1 {
			t.Fatalf("native update resurfaced explicitly retired PR: %d %s", queueAfterNative.Code, queueAfterNative.Body.String())
		}
	}

	// A retired marker blocks additive transfer while hidden. Explicitly
	// selecting the PR clears only that marker and restores its retained body.
	transferPull := transferAdmissionPull(1)
	transferPull.State = "open"
	retiredTransfer := transferRequest(5, TransferGroup{Source: "growth-producer", Pulls: []PullInput{transferPull}})
	if denied := f.call(http.MethodPost, base+"/transfer/preview", f.admin, "", retiredTransfer); denied.Code != http.StatusConflict {
		t.Fatalf("transfer crossed retirement marker: %d %s", denied.Code, denied.Body.String())
	}
	reactivate := queueEditRequest(5, []QueuePullInput{{Number: 1, Title: "Selected pull 1", State: "open", BaseRef: "main", CIState: "success", Mergeability: "mergeable"}}, nil)
	reactivate.Order = []int64{202, 1}
	if response := queueEditHTTP(f, reactivate, "queue-edit-reactivate"); response.Code != 201 {
		t.Fatalf("reactivation failed: %d %s", response.Code, response.Body.String())
	}
	retiredTransfer.ExpectedRevision = int64Pointer(6)
	transferApplyFor(t, f, retiredTransfer, "transfer-after-reactivation")
	// A bounded order does not load or replace the complete 203-entry workspace.
	order := queueEditRequest(7, nil, nil)
	for n := int64(100); n >= 1; n-- {
		order.Order = append(order.Order, n)
	}
	if response := queueEditHTTP(f, order, "large-relative-order"); response.Code != 201 {
		t.Fatalf("large relative order: %d %s", response.Code, response.Body.String())
	}
	var untouched int
	if err := f.store.DB().QueryRow(`SELECT position FROM review_current_pulls WHERE project_id=? AND number=203`, f.project).Scan(&untouched); err != nil || untouched != 202 {
		t.Fatal("unselected append position changed", untouched, err)
	}
	if got := f.call("GET", base+"/queue?limit=1&cursor="+pinned.Cursor, f.readToken, "", nil); got.Code != 200 || got.Body.String() != oldContinuationBefore.Body.String() {
		t.Fatal("relative order changed old cursor", got.Body.String())
	}
	if err := ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}

func TestQueueEditsReplayCASLimitsAndEventRollback(t *testing.T) {
	f := newAPIFixture(t)
	f.observe(t, reviewRequest(0, []PullInput{samplePull(1, "One"), samplePull(2, "Two")}), "queue-edit-seed")
	base := importBase(f)
	valid := queueEditRequest(1, []QueuePullInput{{Number: 1, Title: "One changed", State: "merged", BaseRef: "main", CIState: "success", Mergeability: "mergeable"}}, nil)
	before := importDatabaseState(t, f)
	for _, raw := range []string{
		`{"expected_revision":1,"repository_id":701,"source":"x","queue":null,"retired":[]}`,
		`{"expected_revision":1,"repository_id":701,"source":"x","queue":[{"number":1,"title":"One","state":"open","head_sha":null,"ci_state":"unknown","mergeability":"unknown"}]}`,
		`{"expected_revision":1,"repository_id":701,"source":"x","queue":[{"number":1,"title":"One","state":"open","ci_state":"unknown","mergeability":"unknown"},{"number":1,"title":"Dup","state":"open","ci_state":"unknown","mergeability":"unknown"}]}`,
	} {
		w := f.call(http.MethodPost, base+"/queue/edits", f.admin, "invalid-edit", json.RawMessage(raw))
		if w.Code != 400 || !reflect.DeepEqual(before, importDatabaseState(t, f)) {
			t.Fatalf("invalid queue edit mutated state: %d %s", w.Code, w.Body.String())
		}
	}
	var oversized []QueuePullInput
	for i := 10; i < 111; i++ {
		oversized = append(oversized, QueuePullInput{Number: int64(i), Title: "Extra", State: "open", CIState: "unknown", Mergeability: "unknown"})
	}
	if w := queueEditHTTP(f, queueEditRequest(1, oversized, nil), "oversized-edit"); w.Code != 400 {
		t.Fatalf("oversized selection accepted: %d %s", w.Code, w.Body.String())
	}
	if f.call(http.MethodPost, base+"/queue/edits", f.readToken, "reader-edit", valid).Code != 403 {
		t.Fatal("read-only credential edited queue")
	}
	if w := queueEditHTTP(f, valid, ""); w.Code != 400 {
		t.Fatal("queue edit accepted missing idempotency key")
	}

	if _, err := f.store.DB().Exec(`CREATE TRIGGER queue_edit_event_failure BEFORE INSERT ON events WHEN NEW.event_type='review.observed' BEGIN SELECT RAISE(ABORT,'injected queue edit failure'); END`); err != nil {
		t.Fatal(err)
	}
	failed := queueEditHTTP(f, valid, "queue-edit-failed")
	if failed.Code != 500 || !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatalf("event failure did not rollback edit: %d %s", failed.Code, failed.Body.String())
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER queue_edit_event_failure`); err != nil {
		t.Fatal(err)
	}
	first := queueEditHTTP(f, valid, "queue-edit-cas")
	if first.Code != 201 {
		t.Fatalf("queue edit apply: %d %s", first.Code, first.Body.String())
	}
	later := queueEditRequest(2, []QueuePullInput{{Number: 2, Title: "Two changed", State: "closed", BaseRef: "main", CIState: "failure", Mergeability: "conflicting"}}, nil)
	if response := queueEditHTTP(f, later, "queue-edit-later"); response.Code != 201 {
		t.Fatalf("later edit failed: %d %s", response.Code, response.Body.String())
	}
	stateAfterLater := importDatabaseState(t, f)
	replay := queueEditHTTP(f, valid, "queue-edit-cas")
	if replay.Code != 201 || replay.Body.String() != first.Body.String() || !reflect.DeepEqual(stateAfterLater, importDatabaseState(t, f)) {
		t.Fatalf("replay after later write changed state: %d %s", replay.Code, replay.Body.String())
	}
	stale := queueEditHTTP(f, valid, "queue-edit-stale")
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale revision accepted: %d %s", stale.Code, stale.Body.String())
	}
	queueOnly := f.call(http.MethodPost, base+"/queue/edits", f.admin, "queue-only", json.RawMessage(`{"expected_revision":3,"repository_id":701,"source":"operator","queue":[{"number":1,"title":"One changed","state":"merged","ci_state":"success","mergeability":"mergeable"}]}`))
	if queueOnly.Code != 201 {
		t.Fatalf("omitted retirement array was not normalized empty: %d %s", queueOnly.Code, queueOnly.Body.String())
	}
	retirementOnly := f.call(http.MethodPost, base+"/queue/edits", f.admin, "retirement-only", json.RawMessage(`{"expected_revision":4,"repository_id":701,"source":"operator","retired":[{"number":2,"reason":"defunct"}]}`))
	if retirementOnly.Code != 201 {
		t.Fatalf("omitted queue array was not normalized empty: %d %s", retirementOnly.Code, retirementOnly.Body.String())
	}
	if err := ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}

func TestQueueEditsRelativeOrderPreservesPinnedHistoryAndReplay(t *testing.T) {
	f := newAPIFixture(t)
	retained := samplePull(3, "Three")
	retained.Runs = []RunInput{{SourceRunID: "original-run", Channel: "cli", Scope: "full", Outcome: "unknown", Found: 1, Findings: []Finding{{SourceFindingID: "original-finding", Title: "Original finding", Detail: "Original detail", Severity: "medium", Disposition: "unadjudicated"}}}}
	retained.Attempts = []Attempt{{SourceAttemptID: "original-attempt", Channel: "cli", State: "unknown"}}
	f.observe(t, reviewRequest(0, []PullInput{samplePull(1, "One"), samplePull(2, "Two"), retained, samplePull(4, "Four")}), "order-seed")
	originalState := importDatabaseState(t, f)
	base := importBase(f)
	firstPage := f.call("GET", base+"/queue?limit=1", f.readToken, "", nil)
	var page QueueResponse
	if firstPage.Code != 200 || json.Unmarshal(firstPage.Body.Bytes(), &page) != nil || page.Cursor == "" {
		t.Fatal(firstPage.Body.String())
	}
	pinnedPath := base + "/queue?limit=1&cursor=" + page.Cursor
	pinned := f.call("GET", pinnedPath, f.readToken, "", nil).Body.String()
	detailPath := base + "/pulls/3?snapshot=" + page.SnapshotID
	detail := f.call("GET", detailPath, f.readToken, "", nil).Body.String()
	positions := func() map[int64]int {
		t.Helper()
		rows, err := f.store.DB().Query(`SELECT number,position FROM review_current_pulls WHERE project_id=?`, f.project)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[int64]int{}
		for rows.Next() {
			var n int64
			var p int
			if err := rows.Scan(&n, &p); err != nil {
				t.Fatal(err)
			}
			out[n] = p
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	var versions int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM review_pull_versions WHERE project_id=?`, f.project).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	order := queueEditRequest(1, nil, nil)
	order.Order = []int64{3, 1}
	first := queueEditHTTP(f, order, "relative-order")
	if first.Code != 201 {
		t.Fatalf("order-only: %d %s", first.Code, first.Body.String())
	}
	if got := positions(); !reflect.DeepEqual(got, map[int64]int{1: 2, 2: 1, 3: 0, 4: 3}) {
		t.Fatal(got)
	}
	var afterVersions int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM review_pull_versions WHERE project_id=?`, f.project).Scan(&afterVersions); err != nil || afterVersions != versions {
		t.Fatalf("reorder copied immutable bodies: %d %d %v", versions, afterVersions, err)
	}
	before := importDatabaseState(t, f)
	for _, selected := range [][]int64{{}, {1, 1}, {0}, {-1}, {2147483648}, {99}, make([]int64, 101)} {
		bad := queueEditRequest(2, nil, nil)
		bad.Order = selected
		// Empty order must be sent explicitly; omitempty deliberately preserves old normalized bytes.
		raw, _ := json.Marshal(bad)
		if len(selected) == 0 {
			raw = []byte(`{"expected_revision":2,"repository_id":701,"source":"operator","order":[]}`)
		}
		w := f.call("POST", base+"/queue/edits", f.admin, "bad-order", json.RawMessage(raw))
		if w.Code != 400 || !reflect.DeepEqual(before, importDatabaseState(t, f)) {
			t.Fatalf("invalid order mutated state: %v %d %s", selected, w.Code, w.Body.String())
		}
	}
	for _, raw := range []string{`null`, `[null]`, `[1.0]`, `["1"]`, `true`} {
		w := f.call("POST", base+"/queue/edits", f.admin, "bad-order-wire", json.RawMessage(`{"expected_revision":2,"repository_id":701,"source":"operator","order":`+raw+`}`))
		if w.Code != 400 || !reflect.DeepEqual(before, importDatabaseState(t, f)) {
			t.Fatalf("invalid wire order mutated state: %d %s", w.Code, w.Body.String())
		}
	}
	mixed := queueEditRequest(2, []QueuePullInput{{Number: 5, Title: "Five", State: "open", CIState: "unknown", Mergeability: "unknown"}, {Number: 1, Title: "One edited", State: "open", CIState: "unknown", Mergeability: "unknown"}}, []RetiredPull{{Number: 2, Reason: "defunct"}})
	mixed.Order = []int64{5, 1, 3}
	if _, err := f.store.DB().Exec(`CREATE TRIGGER order_event_failure BEFORE INSERT ON events WHEN NEW.event_type='review.observed' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if w := queueEditHTTP(f, mixed, "mixed-order"); w.Code != 500 || !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatalf("order rollback: %d %s", w.Code, w.Body.String())
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER order_event_failure`); err != nil {
		t.Fatal(err)
	}
	if w := queueEditHTTP(f, mixed, "mixed-order"); w.Code != 201 {
		t.Fatalf("mixed order: %d %s", w.Code, w.Body.String())
	}
	if got := positions(); !reflect.DeepEqual(got, map[int64]int{1: 2, 2: 1, 3: 4, 4: 3, 5: 0}) {
		t.Fatal(got)
	}
	latest := f.call("GET", base+"/queue?limit=100", f.readToken, "", nil)
	if json.Unmarshal(latest.Body.Bytes(), &page) != nil || len(page.Items) != 4 {
		t.Fatal(latest.Body.String())
	}
	for i, n := range []int64{5, 1, 4, 3} {
		if page.Items[i].Number != n {
			t.Fatal(page.Items)
		}
	}
	after := importDatabaseState(t, f)
	for table, rows := range originalState {
		if strings.HasPrefix(table, "review_run_") || strings.HasPrefix(table, "review_finding_") || strings.HasPrefix(table, "review_attempt_") {
			if !reflect.DeepEqual(rows, after[table]) {
				t.Fatalf("order changed retained history table %s", table)
			}
		}
	}
	if w := queueEditHTTP(f, order, "relative-order"); w.Code != 201 || w.Body.String() != first.Body.String() || !reflect.DeepEqual(after, importDatabaseState(t, f)) {
		t.Fatalf("old replay: %d %s", w.Code, w.Body.String())
	}
	order.Order = []int64{1, 3}
	if w := queueEditHTTP(f, order, "relative-order"); w.Code != 409 {
		t.Fatal("changed-key order accepted", w.Code)
	}
	if w := queueEditHTTP(f, order, "stale-order"); w.Code != 409 {
		t.Fatal("stale order accepted", w.Code)
	}
	retired := queueEditRequest(3, nil, nil)
	retired.Order = []int64{2}
	if w := queueEditHTTP(f, retired, "retired-order"); w.Code != 400 {
		t.Fatal("retired order accepted", w.Code)
	}
	retired = queueEditRequest(3, nil, []RetiredPull{{Number: 3, Reason: "defunct"}})
	retired.Order = []int64{3}
	if w := queueEditHTTP(f, retired, "retiring-order"); w.Code != 400 {
		t.Fatal("same-request retired order accepted", w.Code)
	}
	if !reflect.DeepEqual(after, importDatabaseState(t, f)) {
		t.Fatal("refusal changed state")
	}
	if got := f.call("GET", pinnedPath, f.readToken, "", nil); got.Code != 200 || got.Body.String() != pinned {
		t.Fatal("pinned cursor changed", got.Body.String())
	}
	if got := f.call("GET", detailPath, f.readToken, "", nil); got.Code != 200 || got.Body.String() != detail {
		t.Fatal("pinned detail changed", got.Body.String())
	}
	if err := ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}

func TestQueueEditOmittedOrderNormalizationUnchanged(t *testing.T) {
	req := queueEditRequest(7, nil, []RetiredPull{{Number: 2, Reason: "defunct"}})
	if err := validateQueueEdit(&req); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(req)
	if err != nil || string(raw) != `{"expected_revision":7,"repository_id":701,"source":"operator-queue-edit","queue":[],"retired":[{"number":2,"reason":"defunct"}]}` {
		t.Fatalf("legacy normalized bytes changed: %s %v", raw, err)
	}
}

func TestNativeDoesNotResurfaceHistoricallyRetiredMissingParent(t *testing.T) {
	for _, mode := range []string{"queue-edit", "complete-observation"} {
		t.Run(mode, func(t *testing.T) {
			f := newAPIFixture(t)
			one := samplePull(1, "One")
			one.Complete = false
			one.Runs = []RunInput{{SourceRunID: "original-one", Channel: "manual", Scope: "full", Outcome: "unknown"}}
			one.Attempts = []Attempt{{SourceAttemptID: "original-one-attempt", Channel: "manual", State: "unknown"}}
			two := samplePull(2, "Two")
			f.observe(t, reviewRequest(0, []PullInput{one, two}), "historical-retirement-seed")
			base := importBase(f)
			oldQueue := f.call("GET", base+"/queue?limit=1", f.readToken, "", nil)
			var oldPage QueueResponse
			if oldQueue.Code != 200 || json.Unmarshal(oldQueue.Body.Bytes(), &oldPage) != nil || oldPage.Cursor == "" {
				t.Fatalf("could not capture old queue cursor: %d %s", oldQueue.Code, oldQueue.Body.String())
			}
			oldContinuation := f.call("GET", base+"/queue?limit=1&cursor="+oldPage.Cursor, f.readToken, "", nil)
			oldDetail := f.call("GET", base+"/pulls/1?snapshot="+oldPage.SnapshotID, f.readToken, "", nil)
			if oldContinuation.Code != 200 || oldDetail.Code != 200 {
				t.Fatalf("could not capture pinned old data: queue=%d detail=%d", oldContinuation.Code, oldDetail.Code)
			}

			revision := int64(1)
			if mode == "queue-edit" {
				edit := queueEditRequest(revision, nil, []RetiredPull{{Number: 1, Reason: "superseded"}})
				if response := queueEditHTTP(f, edit, "retire-one"); response.Code != 201 {
					t.Fatalf("queue edit retirement failed: %d %s", response.Code, response.Body.String())
				}
				revision++
			}
			// First retain PR1's retirement while replacing the visible parent;
			// a later unrelated full observation closes the current marker.
			f.observe(t, ObservationRequest{ExpectedRevision: &revision, RepositoryID: 701, Source: "complete-source", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []PullInput{two}, Retired: []RetiredPull{{Number: 1, Reason: "superseded"}}}, "retire-one-observation")
			revision++
			f.observe(t, ObservationRequest{ExpectedRevision: &revision, RepositoryID: 701, Source: "complete-source", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []PullInput{}, Retired: []RetiredPull{{Number: 2, Reason: "superseded"}}}, "retire-two-observation")
			revision++
			f.observe(t, ObservationRequest{ExpectedRevision: &revision, RepositoryID: 701, Source: "complete-source", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []PullInput{}, Retired: []RetiredPull{}}, "clear-current-retirements")
			revision++

			scope := transferTestScope(t, f)
			publication := NativePublication{Pull: samplePull(1, "One"), Attempt: &Attempt{SourceAttemptID: "native-after-retirement", Channel: "cli", State: "unknown"}}
			if _, err := f.store.DB().Exec(`CREATE TRIGGER native_retirement_event_failure BEFORE INSERT ON events WHEN NEW.event_type='review.observed' BEGIN SELECT RAISE(ABORT,'injected native event failure'); END`); err != nil {
				t.Fatal(err)
			}
			beforeFailedNative := importDatabaseState(t, f)
			if _, err := f.store.Mutate(t.Context(), scope, core.Mutation{Operation: "test.native.retired", Target: "workspace", Key: "native-failed", Request: publication}, func(ctx context.Context, tx *sql.Tx) (any, error) {
				return PublishNative(ctx, tx, scope, publication)
			}); err == nil || !reflect.DeepEqual(beforeFailedNative, importDatabaseState(t, f)) {
				t.Fatalf("failed native publication did not rollback staged retirement: %v", err)
			}
			if _, err := f.store.DB().Exec(`DROP TRIGGER native_retirement_event_failure`); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.Mutate(t.Context(), scope, core.Mutation{Operation: "test.native.retired", Target: "workspace", Key: "native-after-retirement", Request: publication}, func(ctx context.Context, tx *sql.Tx) (any, error) {
				return PublishNative(ctx, tx, scope, publication)
			}); err != nil {
				t.Fatalf("native publication failed: %v", err)
			}
			current := f.call("GET", base+"/queue?limit=100", f.readToken, "", nil)
			var currentPage QueueResponse
			if current.Code != 200 || json.Unmarshal(current.Body.Bytes(), &currentPage) != nil {
				t.Fatalf("queue after native publication: %d %s", current.Code, current.Body.String())
			}
			for _, item := range currentPage.Items {
				if item.Number == 1 {
					t.Fatalf("native publication resurfaced historically retired PR1 in %s mode", mode)
				}
			}
			oldContinuationAgain := f.call("GET", base+"/queue?limit=1&cursor="+oldPage.Cursor, f.readToken, "", nil)
			oldDetailAgain := f.call("GET", base+"/pulls/1?snapshot="+oldPage.SnapshotID, f.readToken, "", nil)
			if oldContinuationAgain.Code != 200 || oldContinuationAgain.Body.String() != oldContinuation.Body.String() || oldDetailAgain.Code != 200 || oldDetailAgain.Body.String() != oldDetail.Body.String() {
				t.Fatal("native publication changed old cursor or immutable detail bytes")
			}
			var runs, attempts int
			if err := f.store.DB().QueryRow(`SELECT run_count,attempt_count FROM review_current_totals WHERE project_id=? AND pull_number=1`, f.project).Scan(&runs, &attempts); err != nil || runs != 0 || attempts != 1 {
				t.Fatalf("native reintroduction claimed omitted history: runs=%d attempts=%d err=%v", runs, attempts, err)
			}
			if err := ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
				t.Fatal("history after native retirement preservation:", err)
			}
			if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil {
				t.Fatal("summaries after native retirement preservation:", err)
			}
		})
	}

	t.Run("explicit-reactivation", func(t *testing.T) {
		f := newAPIFixture(t)
		pull := samplePull(1, "One")
		f.observe(t, reviewRequest(0, []PullInput{pull}), "reactivate-seed")
		if response := queueEditHTTP(f, queueEditRequest(1, nil, []RetiredPull{{Number: 1, Reason: "superseded"}}), "reactivate-retire"); response.Code != 201 {
			t.Fatalf("retire: %d %s", response.Code, response.Body.String())
		}
		reactivate := QueuePullInput{Number: 1, Title: pull.Title, State: pull.State, BaseRef: pull.BaseRef, CIState: pull.CIState, Mergeability: pull.Mergeability}
		if response := queueEditHTTP(f, queueEditRequest(2, []QueuePullInput{reactivate}, nil), "reactivate-explicit"); response.Code != 201 {
			t.Fatalf("reactivate: %d %s", response.Code, response.Body.String())
		}
		scope := transferTestScope(t, f)
		publication := NativePublication{Pull: pull, Attempt: &Attempt{SourceAttemptID: "native-after-reactivation", Channel: "cli", State: "unknown"}}
		if _, err := f.store.Mutate(t.Context(), scope, core.Mutation{Operation: "test.native.reactivated", Target: "workspace", Key: "native-after-reactivation", Request: publication}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			return PublishNative(ctx, tx, scope, publication)
		}); err != nil {
			t.Fatal("native publication after explicit reactivation:", err)
		}
		response := f.call("GET", importBase(f)+"/queue", f.readToken, "", nil)
		var queue QueueResponse
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &queue) != nil || len(queue.Items) != 1 || queue.Items[0].Number != 1 {
			t.Fatalf("explicitly reactivated PR did not remain visible: %d %s", response.Code, response.Body.String())
		}
	})
}

func int64Pointer(value int64) *int64 { return &value }
