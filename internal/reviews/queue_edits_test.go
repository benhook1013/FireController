package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func queueEditTestScope(t *testing.T, f *apiFixture) core.Scope {
	t.Helper()
	r := httptest.NewRequest("POST", importBase(f)+"/queue/edits", nil)
	r.SetPathValue("project_id", f.project)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	scope, err := f.store.AdminScope(r)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func TestQueueEditsManageNativeQueueBeyondTwoHundred(t *testing.T) {
	f := newAPIFixture(t)
	pulls := make([]PullInput, 100)
	for i := range pulls {
		pulls[i] = nativePull(int64(i + 1))
		pulls[i].State = "open"
	}
	pulls[99].State = "merged"
	pulls[99].Runs = []RunInput{nativeRun("imported-100")}
	f.observe(t, reviewRequest(0, pulls), "queue-growth-import")
	for n := int64(101); n <= 201; n++ {
		a := nativeAttempt(fmt.Sprintf("retained-%d", n), "prepared")
		p := nativePull(n)
		p.State = "open"
		mustNativePublish(t, f, NativePublication{Pull: p, Attempt: &a})
	}
	run := nativeRun("native-100")
	mustNativePublish(t, f, NativePublication{Pull: nativePull(100), Run: &run})
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_current_pulls WHERE project_id=?`, f.project).Scan(&count); err != nil || count != 201 {
		t.Fatal("growth fixture did not cross full-observation ceiling", count, err)
	}
	base := importBase(f)
	var pinned QueueResponse
	nativeRead(t, f, "/queue?limit=1", &pinned)
	pinnedPath := base + "/queue?limit=1&cursor=" + pinned.Cursor
	pinnedBytes := f.call("GET", pinnedPath, f.readToken, "", nil).Body.String()
	detailPath := base + "/pulls/100?snapshot=" + pinned.SnapshotID
	detailBytes := f.call("GET", detailPath, f.readToken, "", nil).Body.String()
	before := importDatabaseState(t, f)
	edit := queueEditRequest(pinned.Revision, []QueuePullInput{{Number: 100, Title: "Selected merged title", State: "merged", HeadSHA: strings.Repeat("b", 40), CIState: "not_required", Mergeability: "mergeable"}, {Number: 202, Title: "New partial PR", State: "open", CIState: "unknown", Mergeability: "unknown"}}, []RetiredPull{{Number: 1, Reason: "defunct"}})
	first := queueEditHTTP(f, edit, "large-queue-edit")
	if first.Code != 201 {
		t.Fatal(first.Code, first.Body.String())
	}
	var receipt Receipt
	if json.Unmarshal(first.Body.Bytes(), &receipt) != nil {
		t.Fatal("invalid edit receipt")
	}
	var selected PullResponse
	nativeRead(t, f, "/pulls/100", &selected)
	if selected.Pull.Title != "Selected merged title" || selected.Pull.State != "merged" || selected.Pull.CIState != "not_required" || selected.Pull.RunCount != 2 || selected.Pull.Complete {
		t.Fatal("metadata edit changed evidence or inherited coverage", selected.Pull)
	}
	var added PullResponse
	nativeRead(t, f, "/pulls/202", &added)
	if added.Pull.Complete || added.Pull.RunCount != 0 || added.Pull.AttemptCount != 0 {
		t.Fatal("new PR manufactured coverage")
	}
	var page QueueResponse
	nativeRead(t, f, "/queue?limit=100", &page)
	seen := map[int64]bool{}
	for {
		for _, p := range page.Items {
			if seen[p.Number] {
				t.Fatal("duplicate queue entry")
			}
			seen[p.Number] = true
		}
		if !page.HasMore {
			break
		}
		nativeRead(t, f, "/queue?limit=100&cursor="+page.Cursor, &page)
	}
	if len(seen) != 201 || seen[1] || !seen[100] || !seen[202] {
		t.Fatal("omitted merged history or unrelated queue entries changed", len(seen), seen[1], seen[100], seen[202])
	}
	after := importDatabaseState(t, f)
	for table, rows := range before {
		if strings.HasPrefix(table, "review_run_") || strings.HasPrefix(table, "review_finding_") || strings.HasPrefix(table, "review_attempt_") {
			if !reflect.DeepEqual(rows, after[table]) {
				t.Fatal("metadata edit changed retained evidence", table)
			}
		}
	}
	var observed string
	if err := f.store.DB().QueryRow(`SELECT observed_at FROM review_snapshots WHERE project_id=? AND snapshot_id=?`, f.project, receipt.ID).Scan(&observed); err != nil || observed != pinned.ObservedAt {
		t.Fatal("queue edit invented source freshness", observed, err)
	}
	if r := f.call("GET", pinnedPath, f.readToken, "", nil); r.Code != 200 || r.Body.String() != pinnedBytes {
		t.Fatal("old continuation changed")
	}
	if r := f.call("GET", detailPath, f.readToken, "", nil); r.Code != 200 || r.Body.String() != detailBytes {
		t.Fatal("old detail changed")
	}
	// Later native capture remains possible without reopening retired visibility.
	a := nativeAttempt("hidden-after-queue-edit", "unknown")
	later := mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Attempt: &a})
	var latest QueueResponse
	nativeRead(t, f, "/queue?limit=1", &latest)
	if latest.Items[0].Number == 1 {
		t.Fatal("native update resurfaced retired PR")
	}
	state := importDatabaseState(t, f)
	if replay := queueEditHTTP(f, edit, "large-queue-edit"); replay.Code != 201 || replay.Body.String() != first.Body.String() || !reflect.DeepEqual(state, importDatabaseState(t, f)) {
		t.Fatal("original replay after advancement wrote state")
	}
	if r := queueEditHTTP(f, queueEditRequest(later.Revision, []QueuePullInput{{Number: 1, Title: "Explicit return", State: "open", CIState: "unknown", Mergeability: "unknown"}}, nil), "explicit-reactivate"); r.Code != 201 {
		t.Fatal(r.Code, r.Body.String())
	}
	nativeRead(t, f, "/queue?limit=1", &latest)
	if latest.Items[0].Number != 1 {
		t.Fatal("explicit reactivation failed")
	}
	for _, validate := range []func(context.Context, HistoryQuery) error{ValidateHistoryReferences, ValidateCurrentSummaries, ValidateCurrentRunOrder} {
		if err := validate(t.Context(), f.store.DB()); err != nil {
			t.Fatal(err)
		}
	}
	// Reopen the owned fixture and recover the exact original key after all
	// intervening visibility and native changes, without replaying their effects.
	var sequence int
	var name, database string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &database); err != nil {
		t.Fatal(err)
	}
	state = importDatabaseState(t, f)
	instance := f.store.InstanceID()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(t.Context(), filepath.Dir(database))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err = EnsureSchema(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	f.store = reopened
	mux := http.NewServeMux()
	Register(mux, reopened)
	f.mux = mux
	if reopened.InstanceID() != instance {
		t.Fatal("restart changed fixture identity")
	}
	if replay := queueEditHTTP(f, edit, "large-queue-edit"); replay.Code != 201 || replay.Body.String() != first.Body.String() || !reflect.DeepEqual(state, importDatabaseState(t, f)) {
		t.Fatal("restart lost original receipt or repeated edit")
	}
	if r := f.call("GET", pinnedPath, f.readToken, "", nil); r.Code != 200 || r.Body.String() != pinnedBytes {
		t.Fatal("restart changed retained queue continuation")
	}
}

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

func TestQueueEditRetirementCanBeOmittedOrRepeatedByFullObservation(t *testing.T) {
	for _, repeat := range []bool{false, true} {
		t.Run(fmt.Sprintf("repeat-%t", repeat), func(t *testing.T) {
			f := newAPIFixture(t)
			one, two, merged := nativePull(1), nativePull(2), samplePull(3, "Retained merged history")
			one.State, two.State = "open", "open"
			one.Runs = []RunInput{nativeRun("imported-before-retirement")}
			f.observe(t, reviewRequest(0, []PullInput{one, two, merged}), "retirement-omit-seed")
			// Historical retirement alone cannot exempt a currently visible PR
			// from the new observation's explicit-removal requirement.
			if r := queueEditHTTP(f, queueEditRequest(1, nil, []RetiredPull{{Number: 2, Reason: "defunct"}}), "retire-visible-later"); r.Code != 201 {
				t.Fatal(r.Code, r.Body.String())
			}
			if r := queueEditHTTP(f, queueEditRequest(2, []QueuePullInput{{Number: 2, Title: two.Title, State: two.State, HeadSHA: two.HeadSHA, BaseRef: two.BaseRef, BaseSHA: two.BaseSHA, CIState: two.CIState, Mergeability: two.Mergeability}}, nil), "reactivate-visible"); r.Code != 201 {
				t.Fatal(r.Code, r.Body.String())
			}
			captured := nativeRun("native-before-retirement")
			publication := mustNativePublish(t, f, NativePublication{Pull: oneWithoutHistory(one), Run: &captured})
			base := importBase(f)
			var old QueueResponse
			nativeRead(t, f, "/queue?limit=1", &old)
			oldCursorPath := base + "/queue?limit=1&cursor=" + old.Cursor
			oldCursor := f.call("GET", oldCursorPath, f.readToken, "", nil).Body.String()
			oldRunPath := base + "/runs/" + runOpaqueID(f.project, nativeSource, captured.SourceRunID) + "?snapshot=" + old.SnapshotID
			oldRun := f.call("GET", oldRunPath, f.readToken, "", nil)
			if oldRun.Code != 200 {
				t.Fatal(oldRun.Code, oldRun.Body.String())
			}
			retire := queueEditHTTP(f, queueEditRequest(publication.Revision, nil, []RetiredPull{{Number: 1, Reason: "superseded"}}), "retire-owner")
			if retire.Code != 201 {
				t.Fatal(retire.Code, retire.Body.String())
			}
			var retired Receipt
			if json.Unmarshal(retire.Body.Bytes(), &retired) != nil {
				t.Fatal("invalid retirement receipt")
			}
			request := reviewRequest(retired.Revision, []PullInput{two, merged})
			if repeat {
				request.Retired = []RetiredPull{{Number: 1, Reason: "superseded"}}
			}
			before := importDatabaseState(t, f)
			for _, test := range []struct {
				name   string
				status int
				change func(*ObservationRequest)
			}{
				{"visible-omission", 400, func(r *ObservationRequest) { r.Queue = []PullInput{merged} }},
				{"unknown-retirement", 400, func(r *ObservationRequest) { r.Retired = append(r.Retired, RetiredPull{Number: 99, Reason: "defunct"}) }},
				{"stale-revision", 409, func(r *ObservationRequest) { v := retired.Revision - 1; r.ExpectedRevision = &v }},
				{"wrong-repository", 400, func(r *ObservationRequest) { r.RepositoryID = 702 }},
			} {
				bad := request
				bad.Retired = append([]RetiredPull(nil), request.Retired...)
				test.change(&bad)
				r := f.call("POST", base+"/observations", f.admin, test.name, bad)
				if r.Code != test.status || !reflect.DeepEqual(before, importDatabaseState(t, f)) {
					t.Fatal(test.name, r.Code, r.Body.String())
				}
			}
			if _, err := f.store.DB().Exec(`CREATE TRIGGER omit_retired_event_failure BEFORE INSERT ON events WHEN NEW.event_type='review.observed' BEGIN SELECT RAISE(ABORT,'synthetic rollback'); END`); err != nil {
				t.Fatal(err)
			}
			failed := f.call("POST", base+"/observations", f.admin, "full-after-retirement", request)
			if failed.Code != 500 || !reflect.DeepEqual(before, importDatabaseState(t, f)) {
				t.Fatal("admitted omission did not reach atomic event rollback", failed.Code, failed.Body.String())
			}
			if _, err := f.store.DB().Exec(`DROP TRIGGER omit_retired_event_failure`); err != nil {
				t.Fatal(err)
			}
			first := f.call("POST", base+"/observations", f.admin, "full-after-retirement", request)
			if first.Code != 201 {
				t.Fatal("already retired owner required another retirement", first.Code, first.Body.String())
			}
			var applied Receipt
			if json.Unmarshal(first.Body.Bytes(), &applied) != nil {
				t.Fatal("invalid observation receipt")
			}
			if repeat {
				// The parent is now absent, but its current retirement marker still
				// positively identifies an allowed explicit repeat.
				next := reviewRequest(applied.Revision, []PullInput{two, merged})
				next.Retired = []RetiredPull{{Number: 1, Reason: "superseded"}}
				r := f.call("POST", base+"/observations", f.admin, "repeat-marker-without-owner", next)
				if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &applied) != nil {
					t.Fatal("known retirement marker could not be repeated", r.Code, r.Body.String())
				}
			}
			clear := reviewRequest(applied.Revision, []PullInput{two, merged})
			f.observe(t, clear, "clear-current-retirement")
			var markers int
			if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_current_retired WHERE project_id=? AND number=1`, f.project).Scan(&markers); err != nil || markers != 0 {
				t.Fatal("full observation did not replace current retirement membership", markers, err)
			}
			// Native reintroduction uses immutable retirement history, even after
			// the complete observation has cleared current parent and marker.
			attempt := nativeAttempt("after-omitted-retired-owner", "prepared")
			mustNativePublish(t, f, NativePublication{Pull: oneWithoutHistory(one), Attempt: &attempt})
			var queue QueueResponse
			nativeRead(t, f, "/queue?limit=100", &queue)
			if len(queue.Items) != 2 || queue.Items[0].Number != 2 || queue.Items[1].Number != 3 {
				t.Fatal("native publication resurfaced retired owner or dropped merged history", queue.Items)
			}
			if r := f.call("GET", oldCursorPath, f.readToken, "", nil); r.Code != 200 || r.Body.String() != oldCursor {
				t.Fatal("old queue cursor changed")
			}
			if r := f.call("GET", oldRunPath, f.readToken, "", nil); r.Code != 200 || r.Body.String() != oldRun.Body.String() {
				t.Fatal("retirement sequence changed original native evidence")
			}
			state := importDatabaseState(t, f)
			replay := f.call("POST", base+"/observations", f.admin, "full-after-retirement", request)
			if replay.Code != 201 || replay.Body.String() != first.Body.String() || !reflect.DeepEqual(state, importDatabaseState(t, f)) {
				t.Fatal("original full-observation replay changed state")
			}
			for _, validate := range []func(context.Context, HistoryQuery) error{ValidateHistoryReferences, ValidateCurrentSummaries, ValidateCurrentRunOrder} {
				if err := validate(t.Context(), f.store.DB()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func oneWithoutHistory(p PullInput) PullInput { p.Runs = nil; p.Attempts = nil; return p }

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

			scope := queueEditTestScope(t, f)
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
		scope := queueEditTestScope(t, f)
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
