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
	if response := queueEditHTTP(f, reactivate, "queue-edit-reactivate"); response.Code != 201 {
		t.Fatalf("reactivation failed: %d %s", response.Code, response.Body.String())
	}
	retiredTransfer.ExpectedRevision = int64Pointer(6)
	transferApplyFor(t, f, retiredTransfer, "transfer-after-reactivation")
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

func int64Pointer(value int64) *int64 { return &value }
