package reviewexec

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviews"
)

func cliPublicationPull(number int64) *reviews.PullInput {
	pull := projectionPull(number)
	return &pull
}

type cliPublicationHTTPTrap struct{ calls atomic.Int64 }

func (trap *cliPublicationHTTPTrap) RoundTrip(*http.Request) (*http.Response, error) {
	trap.calls.Add(1)
	return nil, fmt.Errorf("unexpected HTTP request during local publication")
}

func cliPublicationFixture(t *testing.T, count int, outcome string) (*attemptFixture, Attempt, ReviewEvidence) {
	t.Helper()
	f, a, input := evidenceFixture(t, "cli", true)
	if err := EnsureAdjudicationSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	if err := reviews.EnsureSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	input.Review.Outcome, input.Review.Complete = outcome, outcome == "completed"
	input.Review.ReportedFindings = 0
	original := input.Review.Findings[0]
	input.Review.Findings = nil
	for i := 0; i < count; i++ {
		finding := original
		finding.Title = fmt.Sprintf("Immutable captured finding %d", i)
		input.Review.Findings = append(input.Review.Findings, finding)
	}
	if input.Review.Complete {
		input.Review.ReportedFindings = int64(count)
	}
	r, err := f.e.RecordReviewEvidence(context.Background(), attemptProject, a.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	f.e, err = New(f.store, attemptCapabilities, WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, f.store)
	f.handler = mux
	evidence, err := f.e.ReadReviewEvidence(context.Background(), evidenceReadScope(t, f, attemptProject, f.reader), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, a, evidence
}

func cliPublicationDetail(t *testing.T, f *attemptFixture, number int64, evidenceID string) reviews.RunDetailResponse {
	t.Helper()
	var runs reviews.RunsResponse
	projectionRead(t, f, fmt.Sprintf("/pulls/%d/runs?limit=100", number), &runs)
	for _, run := range runs.Items {
		if run.Source == "native-executor" && run.SourceRunID == evidenceID {
			var detail reviews.RunDetailResponse
			projectionRead(t, f, "/runs/"+run.ID, &detail)
			return detail
		}
	}
	t.Fatal("published native evidence was absent from public run history")
	return reviews.RunDetailResponse{}
}

func TestPublishCLIAuthoritativeSourceThenAdjudicationAndReplay(t *testing.T) {
	f, a, evidence := cliPublicationFixture(t, 2, "completed")
	ctx := context.Background()
	pull := projectionPull(2)
	// Preserve imported merged history and a distinct producer using the same
	// external ID; it cannot satisfy or block this native publication.
	merged := projectionPull(1)
	merged.State = "merged"
	imported := projectionRun(evidence)
	imported.Channel = "manual"
	merged.Runs = []reviews.RunInput{imported}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "publication-history", encoded(reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: evidence.Anchor.RepositoryID, Source: "synthetic-import", ObservedAt: stamp(), Queue: []reviews.PullInput{merged, pull}}))
	if w.Code != 201 {
		t.Fatalf("fixture import: %d %s", w.Code, w.Body.String())
	}
	var beforeMerged reviews.PullResponse
	projectionRead(t, f, "/pulls/1", &beforeMerged)
	var importedRuns reviews.RunsResponse
	projectionRead(t, f, "/pulls/1/runs", &importedRuns)
	var importedDetail reviews.RunDetailResponse
	projectionRead(t, f, "/runs/"+importedRuns.Items[0].ID, &importedDetail)
	oldDigest := projectionDigest(t, f, importedDetail.SnapshotID)
	pull.HeadSHA, pull.Title = strings.Repeat("9", 40), "Fresh metadata after captured head moved"
	journal := attemptState(t, f, a)
	receipts := attemptCount(t, f.store, `SELECT count(*) FROM receipts`)
	first, err := f.e.PublishCLI(ctx, attemptProject, evidence.ID, &pull)
	if err != nil || first.Outcome != "observed" {
		t.Fatal("initial publication failed", first, err)
	}
	detail := cliPublicationDetail(t, f, 2, evidence.ID)
	if detail.Run.HeadSHA != evidence.Anchor.CandidateSHA || detail.Run.Reviewer != a.Provider || detail.Run.Scope != "admitted comparison" || detail.Run.Finalized || !detail.Run.Complete || !detail.Run.Attributable || detail.Run.Accepted != 0 || len(detail.Findings) != 2 || detail.Run.StartedAt != evidence.Capture.StartedAt || detail.Run.FinishedAt != evidence.Capture.FinishedAt {
		t.Fatal("publication fabricated completion/adjudication or lost captured identity", detail)
	}
	for i, finding := range detail.Findings {
		source := evidence.Capture.Review.Findings[i]
		if finding.SourceFindingID != cliProjectionFindingID(evidence.ID, int64(i)) || finding.Title != source.Title || finding.Detail != source.Detail || finding.Disposition != "unadjudicated" {
			t.Fatal("publication replaced immutable finding content", finding)
		}
	}
	// The publication adds only its own review.observed event, not execution or
	// adjudication state, receipts, or completed/finalized source credit.
	if attemptState(t, f, a) != journal || attemptCount(t, f.store, `SELECT count(*) FROM receipts`) != receipts || adjudicationRead(t, f, evidence.ID).Revision != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_adjudication_changes`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 1 {
		t.Fatal("publication settled execution or invented adjudication", journal)
	}
	var actor string
	if err := f.store.DB().QueryRow(`SELECT actor_id FROM events WHERE event_type='review.observed' ORDER BY sequence DESC LIMIT 1`).Scan(&actor); err != nil || actor != "exec-writer-principal" {
		t.Fatal("publication lost original operation attribution", actor, err)
	}
	adjudicationDecide(t, f, evidence.ID, "publication-accept", 0, 0, "accepted")
	detail = cliPublicationDetail(t, f, 2, evidence.ID)
	if detail.Run.Accepted != 1 || detail.Findings[0].Disposition != "accepted" {
		t.Fatal("ordinary adjudication did not update initial publication")
	}
	var current, afterMerged reviews.PullResponse
	projectionRead(t, f, "/pulls/2", &current)
	projectionRead(t, f, "/pulls/1", &afterMerged)
	var afterImported reviews.RunDetailResponse
	projectionRead(t, f, "/runs/"+importedDetail.Run.ID, &afterImported)
	if current.Pull.HeadSHA != pull.HeadSHA || current.Pull.Title != pull.Title || !reflect.DeepEqual(beforeMerged.Pull, afterMerged.Pull) || !reflect.DeepEqual(importedDetail.Run, afterImported.Run) || !reflect.DeepEqual(importedDetail.Findings, afterImported.Findings) || projectionDigest(t, f, importedDetail.SnapshotID) != oldDigest {
		t.Fatal("native publication lost current PR metadata or imported/merged history")
	}
	state, projection := adjudicationState(t, f, a), projectionDigest(t, f, "")
	pull.Title, pull.HeadSHA = "Must not overwrite current facts on retry", strings.Repeat("8", 40)
	replay, err := f.e.PublishCLI(ctx, attemptProject, evidence.ID, &pull)
	if err != nil || replay.ID != detail.SnapshotID || replay.Revision != detail.Revision || replay.Outcome != "observed" || adjudicationState(t, f, a) != state || projectionDigest(t, f, "") != projection {
		t.Fatal("publication retry changed metadata, ledger, receipt, event or snapshot", err)
	}
}

func TestPublishCLIPrepublicationFinalizationAndProofCorrection(t *testing.T) {
	f, a, evidence := cliPublicationFixture(t, 2, "completed")
	ctx := context.Background()
	adjudicationDecide(t, f, evidence.ID, "before-accept", 0, 0, "accepted")
	adjudicationDecide(t, f, evidence.ID, "before-reject", 1, 1, "rejected")
	adjudicationComplete(t, f, a)
	scope := f.scope(t, attemptProject, f.writer, false)
	body, err := f.e.FinalizeReviewEvidence(ctx, scope, "before-finalize", evidence.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(2)})
	adjudicationReceipt(t, body, err, evidence.ID, "finalized", 3)
	body, err = f.e.RecordFindingFix(ctx, scope, "before-fix", evidence.ID, FindingFixRequest{ExpectedRevision: adjudicationRevision(3), Finding: 0, ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("f", 40), ProofNote: "Original retained proof"})
	adjudicationReceipt(t, body, err, evidence.ID, "fix_recorded", 4)
	body, err = f.e.RecordFindingFix(ctx, scope, "before-fix-correction", evidence.ID, FindingFixRequest{ExpectedRevision: adjudicationRevision(4), Finding: 0, ExpectedSHA: adjudicationSHA(strings.Repeat("f", 40)), FixSHA: strings.Repeat("e", 40), ProofNote: "Corrected retained proof"})
	adjudicationReceipt(t, body, err, evidence.ID, "fix_recorded", 5)
	adjudicationDecide(t, f, evidence.ID, "before-decision-correction", 5, 1, "accepted")
	ledger := adjudicationRead(t, f, evidence.ID)
	if _, err := f.e.PublishCLI(ctx, attemptProject, evidence.ID, cliPublicationPull(2)); err != nil {
		t.Fatal(err)
	}
	detail := cliPublicationDetail(t, f, 2, evidence.ID)
	if !detail.Run.Finalized || detail.Run.Found != 2 || detail.Run.Accepted != 1 || detail.Run.Fixed != 1 || detail.Findings[1].Disposition != "accepted" || detail.Findings[0].FixSHA != strings.Repeat("e", 40) || detail.Findings[0].ProofNote != "Corrected retained proof" || detail.Findings[0].Actor != ledger.Findings[0].ActorID || detail.Findings[0].ResolvedAt != ledger.Findings[0].FixedAt || !reflect.DeepEqual(adjudicationRead(t, f, evidence.ID), ledger) {
		t.Fatal("initial publication lost preexisting decisions, corrections or frozen counts", detail)
	}
}

func TestPublishCLIConcurrentRetryCreatesOneNativeObservation(t *testing.T) {
	f, _, evidence := cliPublicationFixture(t, 2, "completed")
	start := make(chan struct{})
	type result struct {
		receipt reviews.Receipt
		err     error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			receipt, err := f.e.PublishCLI(context.Background(), attemptProject, evidence.ID, cliPublicationPull(2))
			results <- result{receipt: receipt, err: err}
		}()
	}
	close(start)
	a, b := <-results, <-results
	if a.err != nil || b.err != nil || a.receipt != b.receipt || a.receipt.Outcome != "observed" || attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_runs`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.observed'`) != 1 {
		t.Fatal("concurrent initial publication did not reconcile exactly once", a, b)
	}
}

func TestPublishCLINilMetadataPreservesCurrentFactsWithoutHTTP(t *testing.T) {
	f, a, evidence := cliPublicationFixture(t, 2, "completed")
	current := projectionPull(2)
	current.Title, current.HeadSHA = "Newer current title", strings.Repeat("9", 40)
	current.BaseRef, current.BaseSHA = "current-base", strings.Repeat("8", 40)
	current.CIState, current.Mergeability = "pending", "conflicting"
	merged := projectionPull(1)
	merged.State = "merged"
	imported := projectionRun(evidence)
	imported.Channel = "manual"
	merged.Runs = []reviews.RunInput{imported}
	observed := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "current-facts-for-local-publish", encoded(reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: evidence.Anchor.RepositoryID, Source: "synthetic-import", ObservedAt: observed, Queue: []reviews.PullInput{merged, current}}))
	if w.Code != 201 {
		t.Fatalf("fixture import: %d %s", w.Code, w.Body.String())
	}
	var before, beforeMerged reviews.PullResponse
	projectionRead(t, f, "/pulls/2", &before)
	projectionRead(t, f, "/pulls/1", &beforeMerged)
	oldSnapshot := projectionDigest(t, f, before.SnapshotID)
	// No provider is configured. Also block/count standard-client HTTP so an
	// accidental metadata fetch cannot silently escape the local fixture.
	trap := &cliPublicationHTTPTrap{}
	oldTransport, oldClient := http.DefaultTransport, http.DefaultClient
	http.DefaultTransport, http.DefaultClient = trap, &http.Client{Transport: trap}
	t.Cleanup(func() { http.DefaultTransport, http.DefaultClient = oldTransport, oldClient })
	journal := attemptState(t, f, a)
	receipt, err := f.e.PublishCLI(context.Background(), attemptProject, evidence.ID, nil)
	if err != nil || receipt.Outcome != "observed" {
		t.Fatal("local current-facts publication failed", receipt, err)
	}
	var after, afterMerged reviews.PullResponse
	projectionRead(t, f, "/pulls/2", &after)
	projectionRead(t, f, "/pulls/1", &afterMerged)
	facts := after.Pull
	facts.RunCount, facts.RecentRuns = before.Pull.RunCount, before.Pull.RecentRuns
	if !reflect.DeepEqual(facts, before.Pull) || after.ObservedAt != observed || !reflect.DeepEqual(afterMerged.Pull, beforeMerged.Pull) || projectionDigest(t, f, before.SnapshotID) != oldSnapshot || attemptState(t, f, a) != journal {
		t.Fatal("nil publication replaced current metadata, history, freshness or journal")
	}
	detail := cliPublicationDetail(t, f, 2, evidence.ID)
	if detail.Run.HeadSHA != evidence.Anchor.CandidateSHA || after.Pull.HeadSHA == detail.Run.HeadSHA {
		t.Fatal("current moved head and captured head were conflated")
	}
	state, digest := adjudicationState(t, f, a), projectionDigest(t, f, "")
	replay, err := f.e.PublishCLI(context.Background(), attemptProject, evidence.ID, nil)
	if err != nil || replay != receipt || adjudicationState(t, f, a) != state || projectionDigest(t, f, "") != digest || trap.calls.Load() != 0 {
		t.Fatal("nil retry emitted effects or fetched HTTP metadata", err, trap.calls.Load())
	}
}

func TestPublishCLINilMetadataMissingCurrentPullRefusesAtomically(t *testing.T) {
	for _, mode := range []string{"unconfigured", "current-other-pull", "historical-target-only"} {
		t.Run(mode, func(t *testing.T) {
			f, a, evidence := cliPublicationFixture(t, 2, "completed")
			if mode != "unconfigured" {
				revision := int64(0)
				var retired []reviews.RetiredPull
				if mode == "historical-target-only" {
					if _, err := f.e.PublishCLI(context.Background(), attemptProject, evidence.ID, cliPublicationPull(2)); err != nil {
						t.Fatal(err)
					}
					revision = 1
					retired = []reviews.RetiredPull{{Number: 2, Reason: "superseded"}}
				}
				other := projectionPull(1)
				other.State = "merged"
				w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "current-without-target", encoded(reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: evidence.Anchor.RepositoryID, Source: "synthetic-import", ObservedAt: stamp(), Queue: []reviews.PullInput{other}, Retired: retired}))
				if w.Code != 201 {
					t.Fatalf("fixture import: %d %s", w.Code, w.Body.String())
				}
			}
			state, digest := adjudicationState(t, f, a), projectionDigest(t, f, "")
			receipt, err := f.e.PublishCLI(context.Background(), attemptProject, evidence.ID, nil)
			attemptCode(t, err, "review_projection_missing")
			if receipt != (reviews.Receipt{}) || adjudicationState(t, f, a) != state || projectionDigest(t, f, "") != digest {
				t.Fatal("missing current target invented metadata or emitted effects")
			}
		})
	}
}

func TestPublishCLIIncompleteCapturesRemainExplicitlyIncomplete(t *testing.T) {
	for _, outcome := range []string{"uncertain", "failed", "cancelled", "rate_limited"} {
		t.Run(outcome, func(t *testing.T) {
			f, _, evidence := cliPublicationFixture(t, 2, outcome)
			if evidence.Capture.Review.ReportedFindings != 0 {
				t.Fatal("fixture must retain partial findings without a terminal count")
			}
			adjudicationDecide(t, f, evidence.ID, "partial-decision", 0, 0, "accepted")
			if _, err := f.e.PublishCLI(context.Background(), attemptProject, evidence.ID, cliPublicationPull(2)); err != nil {
				t.Fatal(err)
			}
			detail := cliPublicationDetail(t, f, 2, evidence.ID)
			if detail.Run.Found != 2 || detail.Run.Accepted != 1 || detail.Run.Outcome != outcome || detail.Run.Complete || detail.Run.Attributable || detail.Run.Finalized || len(detail.Run.CoverageLimits) == 0 || len(detail.Findings) != 2 {
				t.Fatal("partial source invented completed credit or lost actual findings", detail)
			}
		})
	}
}

func TestPublishCLIRefusesMismatchedBindingsAndBodiesAtomically(t *testing.T) {
	for _, mode := range []string{"project", "pull", "repository", "anchor", "operation", "fingerprint", "id", "runs", "attempts", "invalid-title", "invalid-state"} {
		t.Run(mode, func(t *testing.T) {
			f, a, evidence := cliPublicationFixture(t, 2, "completed")
			project, id, pull := attemptProject, evidence.ID, projectionPull(2)
			code := "observation_conflict"
			switch mode {
			case "project":
				project, code = otherAttemptProject, "not_found"
			case "pull":
				pull.Number = 4
			case "repository":
				if _, err := f.store.DB().Exec(`UPDATE projects SET github_repository_id=999 WHERE id=?`, project); err != nil {
					t.Fatal(err)
				}
			case "anchor":
				anchor := *a.Anchor
				anchor.PatchSHA256 = strings.Repeat("1", 64)
				if _, err := f.store.DB().Exec(`UPDATE review_execution_attempts SET anchor_json=? WHERE id=?`, encoded(anchor), a.ID); err != nil {
					t.Fatal(err)
				}
			case "operation":
				if _, err := f.store.DB().Exec(`UPDATE review_operations SET attempt_id='different-attempt' WHERE id=?`, a.OperationID); err != nil {
					t.Fatal(err)
				}
			case "fingerprint":
				if _, err := f.store.DB().Exec(`UPDATE review_operations SET capability_fingerprint=? WHERE id=?`, strings.Repeat("1", 64), a.OperationID); err != nil {
					t.Fatal(err)
				}
			case "id":
				id, code = "review_evidence_BAD", "validation_failed"
			case "runs":
				pull.Runs, code = []reviews.RunInput{projectionRun(evidence)}, "validation_failed"
			case "attempts":
				pull.Attempts, code = []reviews.Attempt{{SourceAttemptID: "injected"}}, "validation_failed"
			case "invalid-title":
				pull.Title, code = strings.Repeat("x", 501), "validation_failed"
			case "invalid-state":
				pull.State, code = "invented", "validation_failed"
			}
			state, projection := adjudicationState(t, f, a), projectionDigest(t, f, "")
			receipt, err := f.e.PublishCLI(context.Background(), project, id, &pull)
			attemptCode(t, err, code)
			if receipt != (reviews.Receipt{}) || adjudicationState(t, f, a) != state || projectionDigest(t, f, "") != projection {
				t.Fatal("refused publication committed partial effects")
			}
		})
	}
}

func TestPublishCLIReplayRejectsChangedAndHistoricalOnlyNativeSources(t *testing.T) {
	for _, mode := range []string{"text", "finding-id", "head", "counts", "decision", "historical-only"} {
		t.Run(mode, func(t *testing.T) {
			f, a, evidence := cliPublicationFixture(t, 2, "completed")
			if _, err := f.e.PublishCLI(context.Background(), attemptProject, evidence.ID, cliPublicationPull(2)); err != nil {
				t.Fatal(err)
			}
			var err error
			switch mode {
			case "text":
				_, err = f.store.DB().Exec(`UPDATE review_findings SET detail='replaced' WHERE project_id=?`, attemptProject)
			case "finding-id":
				_, err = f.store.DB().Exec(`UPDATE review_findings SET source_finding_id='replaced' WHERE project_id=? AND position=0`, attemptProject)
			case "head":
				_, err = f.store.DB().Exec(`UPDATE review_runs SET head_sha=? WHERE project_id=?`, strings.Repeat("9", 40), attemptProject)
			case "counts":
				_, err = f.store.DB().Exec(`UPDATE review_runs SET accepted=1 WHERE project_id=?`, attemptProject)
			case "decision":
				_, err = f.store.DB().Exec(`UPDATE review_findings SET disposition='accepted' WHERE project_id=? AND position=0`, attemptProject)
			case "historical-only":
				w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "replace-current-with-import", encoded(reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(1), RepositoryID: evidence.Anchor.RepositoryID, Source: "synthetic-import", ObservedAt: stamp(), Queue: []reviews.PullInput{projectionPull(2)}}))
				if w.Code != 201 {
					t.Fatalf("fixture observation: %d %s", w.Code, w.Body.String())
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			state, projection := adjudicationState(t, f, a), projectionDigest(t, f, "")
			_, err = f.e.PublishCLI(context.Background(), attemptProject, evidence.ID, cliPublicationPull(2))
			attemptCode(t, err, "observation_conflict")
			if adjudicationState(t, f, a) != state || projectionDigest(t, f, "") != projection {
				t.Fatal("refused reconciliation changed stored source")
			}
		})
	}
}

func TestPublishCLIOversizedDetailRollsBackInitialProjection(t *testing.T) {
	f, a, input := evidenceFixture(t, "cli", true)
	if err := EnsureAdjudicationSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	if err := reviews.EnsureSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	original := input.Review.Findings[0]
	input.Review.Findings = nil
	for range 100 {
		finding := original
		finding.Title, finding.Detail = strings.Repeat("<", 500), strings.Repeat("x", 16<<10)
		input.Review.Findings = append(input.Review.Findings, finding)
	}
	input.Review.ReportedFindings = 100
	r, err := f.e.RecordReviewEvidence(context.Background(), attemptProject, a.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	scope := f.scope(t, attemptProject, f.writer, false)
	for i := int64(0); i < 100; i++ {
		body, err := f.e.DecideReviewFinding(context.Background(), scope, fmt.Sprintf("large-decision-%d", i), r.ID, FindingDecisionRequest{ExpectedRevision: adjudicationRevision(i * 2), Finding: i, Disposition: "accepted", Reason: strings.Repeat("<", 2048)})
		adjudicationReceipt(t, body, err, r.ID, "adjudicated", i*2+1)
		body, err = f.e.RecordFindingFix(context.Background(), scope, fmt.Sprintf("large-fix-%d", i), r.ID, FindingFixRequest{ExpectedRevision: adjudicationRevision(i*2 + 1), Finding: i, ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("f", 40), ProofNote: strings.Repeat("<", 2048)})
		adjudicationReceipt(t, body, err, r.ID, "fix_recorded", i*2+2)
	}
	// Demonstrate this fixture exceeds the existing native detail byte cap,
	// rather than relying only on individually valid maximum field lengths.
	evidence, state, err := func() (ReviewEvidence, ReviewAdjudication, error) {
		evidence, err := f.e.ReadReviewEvidence(context.Background(), evidenceReadScope(t, f, attemptProject, f.reader), r.ID)
		return evidence, adjudicationRead(t, f, r.ID), err
	}()
	if err != nil {
		t.Fatal(err)
	}
	run, err := cliPublicationRun(evidence, state, a)
	body, encodeErr := json.Marshal(run)
	if err != nil || encodeErr != nil || len(body) <= 4<<20 {
		t.Fatal("fixture does not exceed existing detail budget", len(body), err, encodeErr)
	}
	before, projection := adjudicationState(t, f, a), projectionDigest(t, f, "")
	receipt, err := f.e.PublishCLI(context.Background(), attemptProject, r.ID, cliPublicationPull(2))
	attemptCode(t, err, "workspace_capacity")
	if receipt != (reviews.Receipt{}) || adjudicationState(t, f, a) != before || projectionDigest(t, f, "") != projection {
		t.Fatal("oversized initial publication committed partial projection/event effects")
	}
}
