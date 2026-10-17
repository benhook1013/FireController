package reviewexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

func projectionFixture(t *testing.T) (*attemptFixture, Attempt, ReviewEvidence) {
	t.Helper()
	f, a, id := adjudicationFixture(t, 2, false)
	if err := reviews.EnsureSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	var err error
	f.e, err = New(f.store, attemptCapabilities, WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, f.store)
	f.handler = mux
	evidence, err := f.e.ReadReviewEvidence(context.Background(), evidenceReadScope(t, f, attemptProject, f.reader), id)
	if err != nil {
		t.Fatal(err)
	}
	return f, a, evidence
}

func projectionPull(number int64) reviews.PullInput {
	return reviews.PullInput{Number: number, Title: fmt.Sprintf("Synthetic PR %d", number), State: "open", HeadSHA: strings.Repeat("a", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40), CIState: "success", Mergeability: "mergeable", Complete: true}
}

func projectionRun(e ReviewEvidence) reviews.RunInput {
	r := reviews.RunInput{SourceRunID: e.ID, Channel: "cli", HeadSHA: e.Anchor.CandidateSHA, Reviewer: "Synthetic retained reviewer", Scope: "exact patch", Outcome: e.Capture.Review.Outcome, Attributable: true, Complete: e.Capture.Review.Complete, StartedAt: e.Capture.StartedAt, FinishedAt: e.Capture.FinishedAt, Found: e.Capture.Review.ReportedFindings, CoverageLimits: []string{"Synthetic source limitation"}}
	for i, f := range e.Capture.Review.Findings {
		r.Findings = append(r.Findings, reviews.Finding{SourceFindingID: projectionFindingID(e.ID, int64(i)), Title: f.Title, Detail: f.Detail, Severity: f.Severity, Disposition: "unadjudicated"})
	}
	return r
}

func projectionPublish(t *testing.T, f *attemptFixture, p reviews.NativePublication) reviews.Receipt {
	t.Helper()
	var receipt reviews.Receipt
	err := f.store.BackgroundWrite(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		receipt, err = reviews.PublishNative(ctx, tx, core.Scope{ProjectID: attemptProject, PrincipalID: "synthetic-native-publisher"}, p)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func projectionRead(t *testing.T, f *attemptFixture, route string, dst any) []byte {
	t.Helper()
	w := f.request("GET", "/api/v1/projects/"+attemptProject+"/reviews"+route, f.reader, "", "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), dst) != nil {
		t.Fatalf("read %s: %d %s", route, w.Code, w.Body.String())
	}
	return append([]byte(nil), w.Body.Bytes()...)
}

// Hash complete stored rows, including snapshots not reachable from current,
// so rollback and retained-history assertions cover more than public counts.
func projectionDigest(t *testing.T, f *attemptFixture, snapshot string) string {
	t.Helper()
	h := sha256.New()
	tables := []string{"review_snapshots", "review_pulls", "review_runs", "review_findings", "review_attempts", "review_retired_pulls"}
	if snapshot == "" {
		tables = append(tables, "review_workspace")
	}
	for _, table := range tables {
		query, args := "SELECT * FROM "+table+" WHERE project_id=?", []any{attemptProject}
		if snapshot != "" {
			query, args = query+" AND snapshot_id=?", append(args, snapshot)
		}
		rows, err := f.store.DB().Query(query+" ORDER BY rowid", args...)
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
			values, pointers := make([]any, len(columns)), make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			body, err := json.Marshal(values)
			if err != nil {
				rows.Close()
				t.Fatal(err)
			}
			h.Write(body)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func TestCLIProjectionAdjudicationPreservesFrozenSourceAndHistory(t *testing.T) {
	f, a, evidence := projectionFixture(t)
	ctx := context.Background()
	// A real administrator import remains a distinct historical producer even
	// when its external run ID equals the native evidence ID.
	importedRun := projectionRun(evidence)
	importedRun.Channel = "manual"
	merged := projectionPull(1)
	merged.State, merged.Runs = "merged", []reviews.RunInput{importedRun}
	request := reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: evidence.Anchor.RepositoryID, Source: "synthetic-import", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{merged, projectionPull(2)}}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "import-history", encoded(request))
	if w.Code != 200 && w.Code != 201 {
		t.Fatalf("historical import: %d %s", w.Code, w.Body.String())
	}
	// Establish the terminal attempt before the baseline; this test isolates
	// subsequent adjudication projection changes. Lifecycle publication has
	// its own transition and rollback proof.
	adjudicationComplete(t, f, a)
	run := projectionRun(evidence)
	projectionPublish(t, f, reviews.NativePublication{Pull: projectionPull(2), Run: &run})
	current := projectionPull(2)
	current.Title, current.HeadSHA, current.BaseSHA = "New current PR facts", strings.Repeat("9", 40), strings.Repeat("8", 40)
	current.CIState, current.Mergeability = "pending", "conflicting"
	newAttempt := reviews.Attempt{SourceAttemptID: "later-native-attempt", Channel: "cli", HeadSHA: current.HeadSHA, State: "active", StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	initial := projectionPublish(t, f, reviews.NativePublication{Pull: current, Attempt: &newAttempt})
	oldDigest := projectionDigest(t, f, initial.ID)
	var runs reviews.RunsResponse
	projectionRead(t, f, "/pulls/2/runs", &runs)
	if len(runs.Items) != 1 || runs.Items[0].Source != "native-executor" || runs.Items[0].SourceRunID != evidence.ID {
		t.Fatal("native source identity missing", runs)
	}
	runID := runs.Items[0].ID
	var oldDetail reviews.RunDetailResponse
	oldBody := projectionRead(t, f, "/runs/"+runID+"?snapshot="+initial.ID, &oldDetail)
	var oldPull reviews.PullResponse
	projectionRead(t, f, "/pulls/2", &oldPull)
	var oldMerged reviews.PullResponse
	projectionRead(t, f, "/pulls/1", &oldMerged)
	projectionRead(t, f, "/pulls/1/runs", &runs)
	if len(runs.Items) != 1 || runs.Items[0].Source != "synthetic-import" || runs.Items[0].ID == runID {
		t.Fatal("import and native identity collapsed", runs)
	}
	importedID := runs.Items[0].ID
	var oldImported reviews.RunDetailResponse
	projectionRead(t, f, "/runs/"+importedID, &oldImported)
	var sourceBody string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE id=?`, evidence.ID).Scan(&sourceBody); err != nil {
		t.Fatal(err)
	}

	adjudicationDecide(t, f, evidence.ID, "projection-accept", 0, 0, "accepted")
	adjudicationDecide(t, f, evidence.ID, "projection-reject", 1, 1, "rejected")
	var decided reviews.RunDetailResponse
	projectionRead(t, f, "/runs/"+runID, &decided)
	if decided.Run.Accepted != 1 || decided.Run.Finalized || decided.Findings[1].Disposition != "rejected" {
		t.Fatal("authoritative decisions not projected", decided)
	}
	journal := attemptState(t, f, a)
	scope := f.scope(t, attemptProject, f.writer, false)
	body, err := f.e.FinalizeReviewEvidence(ctx, scope, "projection-finalize", evidence.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(2)})
	adjudicationReceipt(t, body, err, evidence.ID, "finalized", 3)
	var finalized reviews.RunDetailResponse
	projectionRead(t, f, "/runs/"+runID, &finalized)
	if !finalized.Run.Finalized || finalized.Run.Found != 2 || finalized.Run.Accepted != 1 || finalized.Run.Fixed != 0 {
		t.Fatal("finalization did not project original counts before subsequent proof", finalized)
	}
	finalDigest := projectionDigest(t, f, finalized.SnapshotID)
	fix := FindingFixRequest{ExpectedRevision: adjudicationRevision(3), Finding: 0, ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("f", 40), ProofNote: "Focused synthetic proof <b>literal</b>"}
	body, err = f.e.RecordFindingFix(ctx, scope, "projection-fix", evidence.ID, fix)
	adjudicationReceipt(t, body, err, evidence.ID, "fix_recorded", 4)
	correction := FindingDecisionRequest{ExpectedRevision: adjudicationRevision(4), Finding: 1, Disposition: "accepted", Reason: "Correct only the still-unfixed finding"}
	body, err = f.e.DecideReviewFinding(ctx, scope, "projection-correct", evidence.ID, correction)
	first := adjudicationReceipt(t, body, err, evidence.ID, "adjudicated", 5)
	ledger := adjudicationRead(t, f, evidence.ID)
	var detail reviews.RunDetailResponse
	projectionRead(t, f, "/runs/"+runID, &detail)
	if !detail.Run.Finalized || detail.Run.Found != 2 || detail.Run.Accepted != 1 || detail.Run.Routed != 0 || detail.Run.Fixed != 1 || ledger.FrozenCounts == nil || *ledger.FrozenCounts != (FindingCounts{Found: 2, Accepted: 1}) {
		t.Fatal("correction rewrote frozen counts or lost proof", ledger, detail)
	}
	if detail.Revision != initial.Revision+5 || ledger.Revision != 5 || attemptCount(t, f.store, `SELECT count(*) FROM review_adjudication_changes WHERE evidence_id=?`, evidence.ID) != 5 {
		t.Fatal("successful mutations did not each advance ledger and projection once")
	}
	for i, finding := range detail.Findings {
		original, decision := oldDetail.Findings[i], ledger.Findings[i]
		if finding.SourceFindingID != original.SourceFindingID || finding.Title != original.Title || finding.Detail != original.Detail || finding.Severity != original.Severity || finding.Disposition != decision.Disposition || finding.DecisionReason != decision.Reason || finding.Actor != decision.ActorID || finding.DecidedAt != decision.DecidedAt || finding.FixSHA != decision.FixSHA || finding.ProofNote != decision.ProofNote || finding.ResolvedAt != decision.FixedAt {
			t.Fatal("projection diverged from ledger or replaced immutable text", finding, decision)
		}
	}
	unchangedSummary := detail.Run
	unchangedSummary.Accepted, unchangedSummary.Finalized, unchangedSummary.Fixed = oldDetail.Run.Accepted, oldDetail.Run.Finalized, oldDetail.Run.Fixed
	if !reflect.DeepEqual(unchangedSummary, oldDetail.Run) {
		t.Fatal("projection rewrote original capture metadata")
	}
	var pull, mergedNow reviews.PullResponse
	projectionRead(t, f, "/pulls/2", &pull)
	projectionRead(t, f, "/pulls/1", &mergedNow)
	// Source freshness and current PR metadata are not review decisions.
	if pull.Pull.Title != current.Title || pull.Pull.HeadSHA != current.HeadSHA || pull.Pull.BaseSHA != current.BaseSHA || pull.Pull.CIState != current.CIState || pull.Pull.Mergeability != current.Mergeability || !reflect.DeepEqual(projectionAttempts(t, f, 2, pull.SnapshotID), projectionAttempts(t, f, 2, oldPull.SnapshotID)) || pull.ObservedAt != oldPull.ObservedAt || !reflect.DeepEqual(mergedNow.Pull, oldMerged.Pull) || mergedNow.Pull.State != "merged" {
		t.Fatal("current facts, merged history, or conservative freshness changed")
	}
	var queue reviews.QueueResponse
	projectionRead(t, f, "/queue", &queue)
	if len(queue.Items) != 2 || queue.Revision != detail.Revision || queue.SnapshotID != detail.SnapshotID || queue.ObservedAt != oldPull.ObservedAt {
		t.Fatal("queue did not retain current snapshot and conservative observation", queue)
	}
	for _, item := range queue.Items {
		if item.Number == 1 && !reflect.DeepEqual(item, oldMerged.Pull) {
			t.Fatal("queue changed imported merged history")
		}
		if item.Number == 2 && (len(item.RecentRuns) != 1 || !reflect.DeepEqual(item.RecentRuns[0], detail.Run)) {
			t.Fatal("compact queue summary differs from authoritative projected detail", item)
		}
	}
	var imported, historical reviews.RunDetailResponse
	projectionRead(t, f, "/runs/"+importedID, &imported)
	if !reflect.DeepEqual(imported.Run, oldImported.Run) || !reflect.DeepEqual(imported.Findings, oldImported.Findings) || projectionDigest(t, f, initial.ID) != oldDigest || projectionDigest(t, f, finalized.SnapshotID) != finalDigest || !bytes.Equal(oldBody, projectionRead(t, f, "/runs/"+runID+"?snapshot="+initial.ID, &historical)) {
		t.Fatal("imported source or historical snapshots changed")
	}
	var retained string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE id=?`, evidence.ID).Scan(&retained); err != nil || retained != sourceBody || attemptState(t, f, a) != journal {
		t.Fatal("adjudication changed original ledger capture or settled execution", err)
	}
	state, projection := adjudicationState(t, f, a), projectionDigest(t, f, "")
	replay, err := f.e.DecideReviewFinding(ctx, scope, "projection-correct", evidence.ID, correction)
	if err != nil || !bytes.Equal(first, replay) || adjudicationState(t, f, a) != state || projectionDigest(t, f, "") != projection {
		t.Fatal("exact replay emitted effects or cloned a snapshot", err)
	}
}

func TestCLIProjectionIdentityConflictRollsBackLedgerAndProjection(t *testing.T) {
	f, a, evidence := projectionFixture(t)
	run := projectionRun(evidence)
	run.Findings[1].SourceFindingID = "different-native-source-finding"
	projectionPublish(t, f, reviews.NativePublication{Pull: projectionPull(2), Run: &run})
	before, projection := adjudicationState(t, f, a), projectionDigest(t, f, "")
	body, err := f.e.DecideReviewFinding(context.Background(), f.scope(t, attemptProject, f.writer, false), "conflicting-projection", evidence.ID, FindingDecisionRequest{ExpectedRevision: adjudicationRevision(0), Finding: 0, Disposition: "accepted", Reason: "Must not survive failed projection"})
	attemptCode(t, err, "observation_conflict")
	if len(body) != 0 || adjudicationRead(t, f, evidence.ID).Revision != 0 || adjudicationState(t, f, a) != before || projectionDigest(t, f, "") != projection {
		t.Fatal("projection conflict committed ledger/audit/event/receipt/snapshot effects")
	}
}

func TestCLIProjectionMissingInitialRunRetainsLedgerOnly(t *testing.T) {
	f, a, evidence := projectionFixture(t)
	before := projectionDigest(t, f, "")
	adjudicationDecide(t, f, evidence.ID, "ledger-without-projection", 0, 0, "accepted")
	ledger := adjudicationRead(t, f, evidence.ID)
	if ledger.Revision != 1 || ledger.Findings[0].Disposition != "accepted" || ledger.Findings[0].ActorID != "exec-writer-principal" || projectionDigest(t, f, "") != before || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.observed'`) != 0 {
		t.Fatal("missing initial projection invented history or blocked valid adjudication")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_adjudication_changes WHERE evidence_id=?`, evidence.ID) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 1 {
		t.Fatal("ledger-only decision lost audit or settled execution")
	}
}
