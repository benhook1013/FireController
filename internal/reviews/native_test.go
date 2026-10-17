package reviews

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func nativePull(number int64) PullInput {
	p := samplePull(number, fmt.Sprintf("Native fixture PR %d", number))
	p.HeadSHA, p.BaseSHA = strings.Repeat("a", 40), strings.Repeat("b", 40)
	p.Complete = false
	return p
}

func nativeRun(id string) RunInput {
	return RunInput{SourceRunID: id, Channel: "cli", HeadSHA: strings.Repeat("a", 40), Reviewer: "Synthetic native reviewer", Scope: "exact patch", Outcome: "completed", Attributable: true, Complete: true,
		StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", Found: 1, Accepted: 1, Finalized: true, CoverageLimits: []string{"Synthetic source-only limitation"},
		Findings: []Finding{{SourceFindingID: "finding-" + id, Title: "Literal <b>finding</b>", Detail: "RETAINED_NATIVE_DETAIL", Severity: "high", Disposition: "accepted", DecisionReason: "Retained decision", Actor: "Synthetic adjudicator", DecidedAt: "2026-01-01T00:02:00Z", FixSHA: strings.Repeat("c", 40), ProofNote: "RETAINED_NATIVE_FIX", ResolvedAt: "2026-01-01T00:03:00Z"}}}
}

func nativeAttempt(id, state string) Attempt {
	return Attempt{SourceAttemptID: id, Channel: "cli", HeadSHA: strings.Repeat("a", 40), State: state, StartedAt: "2026-01-01T00:00:00Z"}
}

func nativePublish(f *apiFixture, p NativePublication) (Receipt, error) {
	var receipt Receipt
	err := f.store.BackgroundWrite(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		receipt, err = PublishNative(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "synthetic-native-publisher"}, p)
		return err
	})
	return receipt, err
}

func mustNativePublish(t *testing.T, f *apiFixture, p NativePublication) Receipt {
	t.Helper()
	receipt, err := nativePublish(f, p)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func nativeRead(t *testing.T, f *apiFixture, route string, dst any) []byte {
	t.Helper()
	w := f.call(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews"+route, f.readToken, "", nil)
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), dst) != nil {
		t.Fatalf("native read %s: %d %s", route, w.Code, w.Body.String())
	}
	return append([]byte(nil), w.Body.Bytes()...)
}

func nativeErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("wanted %s, got %v", code, err)
	}
}

func nativeSnapshotBytes(t *testing.T, f *apiFixture, snapshot string) string {
	t.Helper()
	hash := sha256.New()
	for _, table := range []string{"review_snapshots", "review_pulls", "review_runs", "review_findings", "review_attempts", "review_retired_pulls"} {
		rows, err := f.store.DB().Query("SELECT * FROM "+table+" WHERE project_id=? AND snapshot_id=? ORDER BY rowid", f.project, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		hash.Write([]byte(table))
		for rows.Next() {
			values, pointers := make([]any, len(columns)), make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			data, err := json.Marshal(values)
			if err != nil {
				rows.Close()
				t.Fatal(err)
			}
			hash.Write(data)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func TestNativePublicationPreservesUnrelatedPullCompleteness(t *testing.T) {
	f := newAPIFixture(t)
	incomplete := nativePull(2)
	incomplete.Runs = []RunInput{nativeRun("complete-run-in-incomplete-observation")}
	knownWithoutRuns := nativePull(4)
	knownWithoutRuns.Complete = true
	knownWithIncompleteRun := nativePull(6)
	knownWithIncompleteRun.Complete = true
	knownWithIncompleteRun.Runs = []RunInput{{SourceRunID: "imported-incomplete-run", Channel: "manual", HeadSHA: knownWithIncompleteRun.HeadSHA, Scope: "Retained partial finding inventory", Outcome: "unknown", Found: 1}}
	old := f.observe(t, reviewRequest(0, []PullInput{incomplete, knownWithoutRuns, knownWithIncompleteRun}), "original-completeness")["id"].(string)
	// Rebuilding all compact rows must still repair an older missing source,
	// without changing this unrelated producer's completeness assertion.
	var stored string
	if err := f.store.DB().QueryRow(`SELECT recent_runs_json FROM review_pulls WHERE project_id=? AND snapshot_id=? AND number=6`, f.project, old).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var summaries []map[string]any
	if json.Unmarshal([]byte(stored), &summaries) != nil || len(summaries) != 1 {
		t.Fatal("old compact fixture")
	}
	delete(summaries[0], "source")
	data, _ := json.Marshal(summaries)
	if _, err := f.store.DB().Exec(`UPDATE review_pulls SET recent_runs_json=? WHERE project_id=? AND snapshot_id=? AND number=6`, string(data), f.project, old); err != nil {
		t.Fatal(err)
	}
	oldDigest := nativeSnapshotBytes(t, f, old)
	before := map[int64]PullResponse{}
	for _, number := range []int64{2, 4, 6} {
		var detail PullResponse
		nativeRead(t, f, fmt.Sprintf("/pulls/%d", number), &detail)
		before[number] = detail
	}
	a := nativeAttempt("targeted-new-pull", "prepared")
	next := mustNativePublish(t, f, NativePublication{Pull: nativePull(9), Attempt: &a})
	for _, number := range []int64{2, 4, 6} {
		var after PullResponse
		nativeRead(t, f, fmt.Sprintf("/pulls/%d", number), &after)
		if !reflect.DeepEqual(before[number].Pull, after.Pull) {
			t.Errorf("targeted publication changed unrelated PR%d source metadata: complete %v -> %v", number, before[number].Pull.Complete, after.Pull.Complete)
		}
	}
	if err := f.store.DB().QueryRow(`SELECT recent_runs_json FROM review_pulls WHERE project_id=? AND snapshot_id=? AND number=6`, f.project, next.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal([]byte(stored), &summaries) != nil || summaries[0]["source"] != "test-import" {
		t.Fatal("full compact origin repair lost")
	}
	if nativeSnapshotBytes(t, f, old) != oldDigest {
		t.Fatal("publication rewrote retained imported snapshot")
	}
}

func TestNativePublicationTargetCompletenessFollowsHeadAndRetainedRuns(t *testing.T) {
	for _, tc := range []struct {
		name                                                 string
		original, incoming, changedHead, incompleteRun, want bool
	}{
		{name: "same-head-preserves-complete", original: true, want: true},
		{name: "same-head-does-not-upgrade-partial", incoming: true},
		{name: "changed-head-clears-complete", original: true, changedHead: true},
		{name: "changed-head-does-not-assert-inventory", original: true, incoming: true, changedHead: true},
		{name: "selected-incomplete-run-clears-complete", original: true, incompleteRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAPIFixture(t)
			original := nativePull(1)
			original.State = "open"
			original.Complete = tc.original
			if tc.incompleteRun {
				original.Runs = []RunInput{{SourceRunID: "target-partial-run", Channel: "manual", HeadSHA: original.HeadSHA, Scope: "Retained incomplete target inventory", Outcome: "unknown", Found: 1}}
			}
			old := f.observe(t, reviewRequest(0, []PullInput{original}), "complete-original")["id"].(string)
			oldDigest := nativeSnapshotBytes(t, f, old)
			update := nativePull(1)
			update.State = "open"
			update.Title = "New selected metadata"
			update.Complete = tc.incoming
			if tc.changedHead {
				update.HeadSHA = strings.Repeat("d", 40)
			}
			attempt := nativeAttempt("target-preparation", "prepared")
			attempt.HeadSHA = update.HeadSHA
			mustNativePublish(t, f, NativePublication{Pull: update, Attempt: &attempt})
			var detail PullResponse
			nativeRead(t, f, "/pulls/1", &detail)
			if detail.Pull.Complete != tc.want || detail.Pull.HeadSHA != update.HeadSHA || detail.Pull.Title != update.Title {
				t.Fatalf("selected metadata has stale/unsupported completeness: got %+v, complete want %v", detail.Pull, tc.want)
			}
			var queue QueueResponse
			nativeRead(t, f, "/queue", &queue)
			if len(queue.Items) != 1 || queue.Items[0].Complete != tc.want {
				t.Fatal("queue completeness differs from public pull detail")
			}
			if nativeSnapshotBytes(t, f, old) != oldDigest {
				t.Fatal("target update rewrote historical snapshot")
			}
		})
	}
}

func TestNativePreservesImportedOriginEvidenceAndOldBytes(t *testing.T) {
	f := newAPIFixture(t)
	imported := nativeRun("shared-external-id")
	imported.Channel, imported.Reviewer = "manual", "Historical producer"
	imported.CoverageLimits = []string{"Exact imported coverage limitation"}
	imported.Findings[0].Detail, imported.Findings[0].ProofNote = "IMPORTED_IMMUTABLE_BODY", "IMPORTED_FIXED_PROOF"
	pull := nativePull(1)
	pull.Complete, pull.Runs = true, []RunInput{imported}
	duration := int64(60)
	pull.Attempts = []Attempt{{SourceAttemptID: "shared-attempt", Channel: "manual", HeadSHA: pull.HeadSHA, State: "completed", StartedAt: imported.StartedAt, FinishedAt: imported.FinishedAt, DurationSeconds: &duration, SourceRunID: imported.SourceRunID}}
	request := reviewRequest(0, []PullInput{pull, nativePull(2)})
	request.Source, request.ObservedAt = " original historical producer ", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	initial := request
	initial.Queue = append(append([]PullInput(nil), request.Queue...), nativePull(7))
	f.observe(t, initial, "before-explicit-retirement")
	revision := int64(1)
	request.ExpectedRevision = &revision
	request.Retired = []RetiredPull{{Number: 7, Reason: "superseded"}}
	old := f.observe(t, request, "import-before-native")["id"].(string)
	// Retained compact bytes can predate the explicit source field.
	var compact []map[string]any
	var stored string
	if err := f.store.DB().QueryRow(`SELECT recent_runs_json FROM review_pulls WHERE project_id=? AND snapshot_id=? AND number=1`, f.project, old).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal([]byte(stored), &compact) != nil {
		t.Fatal("old compact fixture")
	}
	delete(compact[0], "source")
	encoded, _ := json.Marshal(compact)
	if _, err := f.store.DB().Exec(`UPDATE review_pulls SET recent_runs_json=? WHERE project_id=? AND snapshot_id=? AND number=1`, string(encoded), f.project, old); err != nil {
		t.Fatal(err)
	}
	oldDigest := nativeSnapshotBytes(t, f, old)
	importedID := runOpaqueID(f.project, request.Source, imported.SourceRunID)
	oldRoutes := []string{"/pulls/1?snapshot=" + old, "/pulls/1/runs?snapshot=" + old, "/runs/" + importedID + "?snapshot=" + old}
	oldResponses := make([][]byte, len(oldRoutes))
	for i, route := range oldRoutes {
		var value any
		oldResponses[i] = nativeRead(t, f, route, &value)
	}
	var initialQueue QueueResponse
	nativeRead(t, f, "/queue?limit=1", &initialQueue)
	captured := nativeRun(imported.SourceRunID)
	a := nativeAttempt("shared-attempt", "completed")
	a.FinishedAt, a.DurationSeconds, a.SourceRunID, a.TriggerID = captured.FinishedAt, &duration, captured.SourceRunID, "trigger-native"
	metadata := pull
	metadata.Runs, metadata.Attempts = nil, nil
	next := mustNativePublish(t, f, NativePublication{Pull: metadata, Run: &captured, Attempt: &a})
	if next.Revision != 3 || next.ID == old {
		t.Fatal("native update did not append snapshot")
	}
	var queue QueueResponse
	queueBytes := nativeRead(t, f, "/queue", &queue)
	if queue.Source != nativeSource || queue.ObservedAt != request.ObservedAt || queue.Revision != 3 || len(queue.Items) != 2 || queue.Items[0].State != "merged" {
		t.Fatal("publisher refreshed unrelated facts or lost merged history")
	}
	var detail PullResponse
	pullBytes := nativeRead(t, f, "/pulls/1", &detail)
	var runs RunsResponse
	runBytes := nativeRead(t, f, "/pulls/1/runs", &runs)
	if detail.Source != nativeSource || runs.Source != nativeSource || len(runs.Items) != 2 || detail.Pull.RunCount != 2 || detail.Pull.AttemptCount != 2 {
		t.Fatal("mixed native projection lost producer history")
	}
	for _, summaries := range [][]RunSummary{queue.Items[0].RecentRuns, detail.Pull.RecentRuns, runs.Items} {
		if len(summaries) != 2 || summaries[0].Source != nativeSource || summaries[1].Source != request.Source || summaries[1].ID != importedID || summaries[0].ID == summaries[1].ID {
			t.Fatal("same external ID producers collapsed or origin relabeled", summaries)
		}
		if summaries[1].Found != 1 || summaries[1].Accepted != 1 || !summaries[1].Finalized || summaries[1].Fixed != 1 || !reflect.DeepEqual(summaries[1].CoverageLimits, imported.CoverageLimits) {
			t.Fatal("imported counts/limits/fix projection changed")
		}
	}
	var importedDetail, nativeDetail RunDetailResponse
	nativeRead(t, f, "/runs/"+importedID, &importedDetail)
	nativeRead(t, f, "/runs/"+runOpaqueID(f.project, nativeSource, captured.SourceRunID), &nativeDetail)
	if importedDetail.Source != nativeSource || importedDetail.Run.Source != request.Source || !reflect.DeepEqual(importedDetail.Findings, imported.Findings) || nativeDetail.Run.Source != nativeSource || !reflect.DeepEqual(nativeDetail.Findings, captured.Findings) {
		t.Fatal("full finding text/decision/fix or producer identity changed")
	}
	for _, body := range [][]byte{queueBytes, pullBytes, runBytes} {
		if len(body) > 1<<20 || bytes.Contains(body, []byte("IMPORTED_IMMUTABLE_BODY")) || bytes.Contains(body, []byte("RETAINED_NATIVE_DETAIL")) {
			t.Fatal("compact projection expanded findings or exceeded bound")
		}
	}
	if nativeSnapshotBytes(t, f, old) != oldDigest {
		t.Fatal("native publication rewrote old snapshot storage bytes")
	}
	for i, route := range oldRoutes {
		var value any
		if !bytes.Equal(nativeRead(t, f, route, &value), oldResponses[i]) {
			t.Fatal("old explicit snapshot response bytes changed")
		}
	}
	var continued QueueResponse
	nativeRead(t, f, "/queue?limit=1&cursor="+initialQueue.Cursor, &continued)
	if continued.SnapshotID != old || continued.Revision != 2 || len(continued.Items) != 1 || continued.Items[0].Number != 2 {
		t.Fatal("native publish moved retained queue cursor")
	}
	var retained int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_retired_pulls WHERE project_id=? AND snapshot_id=? AND number=7 AND reason='superseded'`, f.project, next.ID).Scan(&retained); err != nil || retained != 1 {
		t.Fatal("retirement provenance was not retained", err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_attempts WHERE project_id=? AND snapshot_id=? AND source_attempt_id='shared-attempt'`, f.project, next.ID).Scan(&retained); err != nil || retained != 2 {
		t.Fatal("same attempt ID from separate producers collapsed", err)
	}
}

func TestNativeFirstPublicationAndExplicitTransactionReplay(t *testing.T) {
	f := newAPIFixture(t)
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("project_id", f.project)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	scope, err := f.store.AdminScope(r)
	if err != nil {
		t.Fatal(err)
	}
	a := nativeAttempt("first-native-attempt", "prepared")
	p := NativePublication{Pull: nativePull(1), Attempt: &a}
	mutation := core.Mutation{Operation: "test.native-publication", Target: "first", Key: "first-native-key", Request: p}
	callbackCalls := 0
	callback := func(ctx context.Context, tx *sql.Tx) (any, error) {
		callbackCalls++
		return PublishNative(ctx, tx, scope, p)
	}
	first, err := f.store.Mutate(context.Background(), scope, mutation, callback)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.store.Mutate(context.Background(), scope, mutation, callback)
	if err != nil || !bytes.Equal(first, replay) || callbackCalls != 1 {
		t.Fatal("caller receipt replay republished native source", err)
	}
	var receipt Receipt
	var compact map[string]any
	if json.Unmarshal(first, &receipt) != nil || json.Unmarshal(first, &compact) != nil || len(compact) != 3 || receipt.Revision != 1 {
		t.Fatal("native publication receipt is not compact")
	}
	var queue QueueResponse
	nativeRead(t, f, "/queue", &queue)
	if !queue.Configured || queue.Source != nativeSource || len(queue.Items) != 1 || queue.Items[0].AttemptCount != 1 || queue.Items[0].RunCount != 0 {
		t.Fatal("first native attempt fabricated a capture or omitted workspace")
	}
	before := nativeSnapshotBytes(t, f, receipt.ID)
	a2 := nativeAttempt("new-target", "prepared")
	second := mustNativePublish(t, f, NativePublication{Pull: nativePull(2), Attempt: &a2})
	nativeRead(t, f, "/queue", &queue)
	if second.Revision != 2 || len(queue.Items) != 2 || queue.Items[0].Number != 1 || queue.Items[1].Number != 2 || nativeSnapshotBytes(t, f, receipt.ID) != before {
		t.Fatal("targeted new pull replaced existing queue or old bytes")
	}
	if countRows(t, f.store, "receipts") != 1 || countRows(t, f.store, "events") != 2 {
		t.Fatal("replay or targeted append created extra receipt/event effects")
	}
}

func TestNativeAttemptLifecycleKeepsSourceIdentity(t *testing.T) {
	f := newAPIFixture(t)
	pull := nativePull(1)
	a := nativeAttempt("lifecycle", "prepared")
	first := mustNativePublish(t, f, NativePublication{Pull: pull, Attempt: &a})
	old := nativeSnapshotBytes(t, f, first.ID)
	a.State, a.TriggerID = "active", "trigger_123"
	mustNativePublish(t, f, NativePublication{Pull: pull, Attempt: &a})
	run := nativeRun("lifecycle-capture")
	duration := int64(60)
	a.State, a.FinishedAt, a.DurationSeconds, a.SourceRunID = "completed", run.FinishedAt, &duration, run.SourceRunID
	last := mustNativePublish(t, f, NativePublication{Pull: pull, Attempt: &a, Run: &run})
	var detail PullResponse
	nativeRead(t, f, "/pulls/1", &detail)
	var archive AttemptsResponse
	nativeRead(t, f, "/pulls/1/attempts", &archive)
	if len(archive.Items) != 1 || archive.Items[0].State != "completed" || archive.Items[0].SourceRunID != run.SourceRunID || archive.Items[0].TriggerID != "trigger_123" || archive.Items[0].DurationSeconds == nil || *archive.Items[0].DurationSeconds != 60 || archive.Items[0].Source != nativeSource || detail.Pull.RunCount != 1 || detail.Pull.AttemptCount != 1 {
		t.Fatal("lifecycle did not update one retained native attempt")
	}

	if nativeSnapshotBytes(t, f, first.ID) != old {
		t.Fatal("attempt update mutated prepared snapshot")
	}
	for _, change := range []string{"channel", "head", "start", "pull"} {
		t.Run(change, func(t *testing.T) {
			changed, metadata := a, pull
			switch change {
			case "channel":
				changed.Channel = "hosted"
			case "head":
				changed.HeadSHA = strings.Repeat("b", 40)
			case "start":
				changed.StartedAt = "2026-01-01T00:00:01Z"
			case "pull":
				metadata = nativePull(2)
			}
			changed.SourceRunID = ""
			before := nativeSnapshotBytes(t, f, last.ID)
			snapshots, events := countRows(t, f.store, "review_snapshots"), countRows(t, f.store, "events")
			_, err := nativePublish(f, NativePublication{Pull: metadata, Attempt: &changed})
			nativeErrorCode(t, err, "observation_conflict")
			if before != nativeSnapshotBytes(t, f, last.ID) || countRows(t, f.store, "review_snapshots") != snapshots || countRows(t, f.store, "events") != events {
				t.Fatal("attempt identity conflict partially published")
			}
		})
	}
}

func TestNativeAttemptsOnlyLinkOwnRunOnExactTarget(t *testing.T) {
	for _, mode := range []string{"foreign_producer", "other_pull", "channel", "head", "valid"} {
		t.Run(mode, func(t *testing.T) {
			f := newAPIFixture(t)
			pull := nativePull(1)
			run := nativeRun("linked-run")
			if mode == "foreign_producer" {
				pull.Runs = []RunInput{run}
				f.observe(t, reviewRequest(0, []PullInput{pull}), "foreign-link")
				pull.Runs = nil
			} else {
				mustNativePublish(t, f, NativePublication{Pull: pull, Run: &run})
			}
			a := nativeAttempt("linked-attempt", "active")
			a.SourceRunID = run.SourceRunID
			switch mode {
			case "other_pull":
				pull = nativePull(2)
			case "channel":
				a.Channel = "hosted"
			case "head":
				a.HeadSHA = strings.Repeat("b", 40)
			}
			snapshots, events := countRows(t, f.store, "review_snapshots"), countRows(t, f.store, "events")
			_, err := nativePublish(f, NativePublication{Pull: pull, Attempt: &a})
			if mode == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				nativeErrorCode(t, err, "validation_failed")
				if countRows(t, f.store, "review_snapshots") != snapshots || countRows(t, f.store, "events") != events {
					t.Fatal("invalid run link partially published")
				}
			}
		})
	}
}

func TestNativeCapturedRunCanNeverBeReplacedOrReused(t *testing.T) {
	for _, mode := range []string{"exact", "finding_text", "counts", "other_pull", "unfinalized", "historical_only"} {
		t.Run(mode, func(t *testing.T) {
			f := newAPIFixture(t)
			pull := nativePull(1)
			run := nativeRun("once-only")
			if mode == "unfinalized" {
				run.Finalized = false
			}
			first := mustNativePublish(t, f, NativePublication{Pull: pull, Run: &run})
			before := nativeSnapshotBytes(t, f, first.ID)
			if mode == "historical_only" {
				// A newer explicit observation can leave the native run only in
				// retained snapshots; that does not free its source identity.
				f.observe(t, reviewRequest(1, []PullInput{pull}), "new-observation-without-old-native-run")
			}
			snapshots, events := countRows(t, f.store, "review_snapshots"), countRows(t, f.store, "events")
			switch mode {
			case "finding_text":
				run.Findings[0].Detail = "replacement text"
			case "counts":
				run.Found = 2
				run.Complete = false
			case "other_pull":
				pull = nativePull(2)
			}
			_, err := nativePublish(f, NativePublication{Pull: pull, Run: &run})
			nativeErrorCode(t, err, "observation_conflict")
			if countRows(t, f.store, "review_snapshots") != snapshots || countRows(t, f.store, "events") != events || nativeSnapshotBytes(t, f, first.ID) != before {
				t.Fatal("reused source run partially rewrote retained evidence")
			}
		})
	}
}

func TestNativeKnownAttemptIdentitiesCannotBeRewritten(t *testing.T) {
	for _, mode := range []string{"run_clear", "run_replace", "trigger_clear", "trigger_replace", "provider_clear", "provider_replace", "settled_rollback"} {
		t.Run(mode, func(t *testing.T) {
			f := newAPIFixture(t)
			pull, captured := nativePull(1), nativeRun("known-capture")
			a := nativeAttempt("known-attempt", "completed")
			a.FinishedAt, a.SourceRunID, a.TriggerID, a.ProviderReviewID = captured.FinishedAt, captured.SourceRunID, "known-trigger", "known-provider-review"
			mustNativePublish(t, f, NativePublication{Pull: pull, Run: &captured, Attempt: &a})
			other := nativeRun("different-valid-capture")
			last := mustNativePublish(t, f, NativePublication{Pull: pull, Run: &other})
			switch mode {
			case "run_clear":
				a.SourceRunID = ""
			case "run_replace":
				a.SourceRunID = other.SourceRunID
			case "trigger_clear":
				a.TriggerID = ""
			case "trigger_replace":
				a.TriggerID = "replacement-trigger"
			case "provider_clear":
				a.ProviderReviewID = ""
			case "provider_replace":
				a.ProviderReviewID = "replacement-provider-review"
			case "settled_rollback":
				a.State = "active"
			}
			before := nativeSnapshotBytes(t, f, last.ID)
			snapshots, events := countRows(t, f.store, "review_snapshots"), countRows(t, f.store, "events")
			_, err := nativePublish(f, NativePublication{Pull: pull, Attempt: &a})
			if err == nil {
				t.Fatal("native publisher rewrote known attempt evidence: " + mode)
			}
			if countRows(t, f.store, "review_snapshots") != snapshots || countRows(t, f.store, "events") != events || nativeSnapshotBytes(t, f, last.ID) != before {
				t.Fatal("known evidence conflict partially published")
			}
		})
	}
}

func TestNativeCapacityAndCallerFailureRollbackEntireTransaction(t *testing.T) {
	for _, mode := range []string{"queue", "pull_runs", "all_runs", "pull_attempts", "all_attempts", "all_findings", "caller_failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newAPIFixture(t)
			queue := []PullInput{nativePull(1)}
			switch mode {
			case "queue":
				for i := int64(2); i <= 100; i++ {
					queue = append(queue, nativePull(i))
				}
			case "pull_runs", "all_runs":
				if mode == "all_runs" {
					queue = append(queue, nativePull(2), nativePull(3))
				}
				for i := range queue {
					count := 100
					if mode == "all_runs" {
						count = 67
						if i == 2 {
							count = 66
						}
					}
					for n := 0; n < count; n++ {
						queue[i].Runs = append(queue[i].Runs, RunInput{SourceRunID: fmt.Sprintf("old-%d-%d", i, n), Channel: "cli", Scope: "stored", Outcome: "unknown"})
					}
				}
			case "pull_attempts", "all_attempts":
				if mode == "all_attempts" {
					queue = append(queue, nativePull(2), nativePull(3))
				}
				for i := range queue {
					count := 100
					if mode == "all_attempts" {
						count = 67
						if i == 2 {
							count = 66
						}
					}
					for n := 0; n < count; n++ {
						queue[i].Attempts = append(queue[i].Attempts, nativeAttempt(fmt.Sprintf("old-%d-%d", i, n), "unknown"))
					}
				}
			case "all_findings":
				for n := 0; n < 10; n++ {
					queue[0].Runs = append(queue[0].Runs, RunInput{SourceRunID: fmt.Sprintf("finding-cap-%d", n), Channel: "cli", Scope: "stored", Outcome: "unknown", Found: 100})
				}
			}
			old := f.observe(t, reviewRequest(0, queue), "capacity-fixture")["id"].(string)
			if mode == "all_findings" {
				// Local fixture setup inserts a valid retained1000-finding snapshot
				// without routing a large historical observation through import.
				for _, r := range queue[0].Runs {
					if _, err := f.store.DB().Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<100) INSERT INTO review_findings(project_id,snapshot_id,pull_number,run_id,position,source_finding_id,title,detail,severity,disposition,decision_reason,actor,decided_at,fix_sha,proof_note,resolved_at) SELECT ?,?,1,?,x-1,'finding-'||x,'Retained','Exact source','unknown','unadjudicated','','','','','','' FROM n`, f.project, old, runOpaqueID(f.project, "test-import", r.SourceRunID)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := f.store.DB().Exec(`CREATE TABLE native_test_journal(note TEXT NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			before := nativeSnapshotBytes(t, f, old)
			counts := map[string]int{}
			for _, table := range []string{"review_snapshots", "review_pulls", "review_runs", "review_findings", "review_attempts", "review_retired_pulls", "events", "receipts"} {
				counts[table] = countRows(t, f.store, table)
			}
			p := NativePublication{Pull: nativePull(1)}
			a := nativeAttempt("new-capacity-attempt", "prepared")
			r := nativeRun("new-capacity-run")
			switch mode {
			case "queue":
				p.Pull = nativePull(101)
				p.Attempt = &a
			case "pull_runs", "all_findings":
				p.Run = &r
			case "all_runs":
				p.Pull = nativePull(3)
				p.Run = &r
			case "all_attempts":
				p.Pull = nativePull(3)
				p.Attempt = &a
			default:
				p.Attempt = &a
			}
			err := f.store.BackgroundWrite(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, `INSERT INTO native_test_journal(note) VALUES('staged attempt')`); err != nil {
					return err
				}
				if _, err := PublishNative(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "synthetic-publisher"}, p); err != nil {
					return err
				}
				if mode == "caller_failure" {
					return core.Fail("test_failure", "Synthetic journal failure.", 409)
				}
				return nil
			})
			code := "workspace_capacity"
			if mode == "queue" {
				code = "validation_failed"
			}
			if mode == "caller_failure" {
				code = "test_failure"
			}
			nativeErrorCode(t, err, code)
			if nativeSnapshotBytes(t, f, old) != before || countRows(t, f.store, "native_test_journal") != 0 {
				t.Fatal("failed publication changed old bytes or caller journal")
			}
			for table, count := range counts {
				if countRows(t, f.store, table) != count {
					t.Fatal("failed publication left partial rows", table)
				}
			}
			var current string
			if err := f.store.DB().QueryRow(`SELECT snapshot_id FROM review_workspace WHERE project_id=?`, f.project).Scan(&current); err != nil || current != old {
				t.Fatal("failed publication advanced current pointer", err)
			}
		})
	}
}

func TestNativePublicationRunRowsBindOwningSnapshotRevision(t *testing.T) {
	f := newAPIFixture(t)
	imported := nativeRun("retained-historical-run")
	imported.Channel = "manual"
	pull := nativePull(1)
	pull.Runs = []RunInput{imported}
	observation := reviewRequest(0, []PullInput{pull})
	first := f.observe(t, observation, "revision-import-first")["id"].(string)
	firstBytes := nativeSnapshotBytes(t, f, first)
	// A later source observation corrects fix evidence while preserving finalized
	// counts and source text. Native copies must retain this current projection.
	revision := int64(1)
	observation.ExpectedRevision = &revision
	observation.Queue[0].Runs[0].Findings[0].FixSHA = strings.Repeat("d", 40)
	observation.Queue[0].Runs[0].Findings[0].ProofNote = "Corrected retained fix proof"
	corrected := f.observe(t, observation, "revision-import-corrected")["id"].(string)
	correctedBytes := nativeSnapshotBytes(t, f, corrected)
	run := nativeRun("new-native-run")
	appended := mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
	appendedBytes := nativeSnapshotBytes(t, f, appended.ID)
	attempt := nativeAttempt("copy-current-projection", "prepared")
	copied := mustNativePublish(t, f, NativePublication{Pull: nativePull(2), Attempt: &attempt})
	if appended.Revision != 3 || copied.Revision != 4 {
		t.Fatal("native publication did not advance snapshots")
	}
	rows, err := f.store.DB().Query(`SELECT r.snapshot_id,r.snapshot_revision,s.revision,count(*) FROM review_runs r JOIN review_snapshots s ON s.project_id=r.project_id AND s.snapshot_id=r.snapshot_id WHERE r.project_id=? GROUP BY r.snapshot_id,r.snapshot_revision,s.revision ORDER BY s.revision`, f.project)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]int{first: 1, corrected: 1, appended.ID: 2, copied.ID: 2}
	for rows.Next() {
		var snapshot string
		var stored, owning int64
		var count int
		if err := rows.Scan(&snapshot, &stored, &owning, &count); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		want, ok := expected[snapshot]
		if !ok || stored != owning || count != want {
			rows.Close()
			t.Fatalf("copied/new run has wrong owning revision: snapshot=%s stored=%d owning=%d count=%d", snapshot, stored, owning, count)
		}
		delete(expected, snapshot)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(expected) != 0 {
		t.Fatal("publication lost retained run rows", expected)
	}
	var detail RunDetailResponse
	nativeRead(t, f, "/runs/"+runOpaqueID(f.project, observation.Source, imported.SourceRunID), &detail)
	if !detail.Run.Finalized || detail.Run.Found != 1 || detail.Run.Accepted != 1 || detail.Findings[0].FixSHA != strings.Repeat("d", 40) || detail.Findings[0].ProofNote != "Corrected retained fix proof" {
		t.Fatal("copy lost finalized counts or corrected fix projection")
	}
	nativeRead(t, f, "/runs/"+runOpaqueID(f.project, nativeSource, run.SourceRunID), &detail)
	if !reflect.DeepEqual(detail.Findings, run.Findings) {
		t.Fatal("new native row lost retained finding/decision/fix")
	}
	if nativeSnapshotBytes(t, f, first) != firstBytes || nativeSnapshotBytes(t, f, corrected) != correctedBytes || nativeSnapshotBytes(t, f, appended.ID) != appendedBytes {
		t.Fatal("publication rewrote historical snapshot bytes")
	}
	// A structurally valid extra row that omits the new field must be rejected,
	// rather than silently becoming eligible under another snapshot's revision.
	before := nativeSnapshotBytes(t, f, copied.ID)
	_, err = f.store.DB().Exec(`INSERT INTO review_runs(project_id,snapshot_id,pull_number,run_order,id,source,source_run_id,channel,head_sha,reviewer,scope,outcome,attributable,complete,started_at,finished_at,found,accepted,routed,finalized,fixed,coverage_limits_json) SELECT project_id,snapshot_id,pull_number,run_order+10,'omitted-owning-revision',source,'omitted-owning-source',channel,head_sha,reviewer,scope,outcome,attributable,complete,started_at,finished_at,found,accepted,routed,finalized,fixed,coverage_limits_json FROM review_runs WHERE project_id=? AND snapshot_id=? AND source=? AND source_run_id=?`, f.project, copied.ID, nativeSource, run.SourceRunID)
	if err == nil {
		t.Fatal("omitted snapshot revision was accepted")
	}
	if _, err := f.store.DB().Exec(`UPDATE review_runs SET snapshot_revision=snapshot_revision-1 WHERE project_id=? AND snapshot_id=?`, f.project, copied.ID); err == nil {
		t.Fatal("wrong owning snapshot revision update was accepted")
	}
	if nativeSnapshotBytes(t, f, copied.ID) != before {
		t.Fatal("rejected revision mismatch changed snapshot bytes")
	}
}
