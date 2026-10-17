package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func coverageFixture(t *testing.T, count int, partial bool) (*attemptFixture, Attempt, string, SourcePlan) {
	t.Helper()
	f, a, in := evidenceFixture(t, "cli", false)
	if err := EnsureAdjudicationSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	in.ExpectedAnchor.ParentSHA = in.ExpectedAnchor.BaseSHA
	var err error
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, in.ExpectedAnchor, "")
	if err != nil {
		t.Fatal(err)
	}
	original := in.Review.Findings[0]
	in.Review.Findings = make([]reviewprovider.ParsedFinding, count)
	for i := range in.Review.Findings {
		in.Review.Findings[i] = original
	}
	in.Review.ReportedFindings = int64(count)
	if partial {
		in.Review.Outcome, in.Review.Complete = "failed", false
	}
	receipt, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	current := SourcePlan{Repository: reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, Pull: reviewprovider.Pull{Number: 2, RepositoryID: 801, State: "open", HeadSHA: in.ExpectedAnchor.CandidateSHA, BaseSHA: in.ExpectedAnchor.BaseSHA, BaseRef: in.ExpectedAnchor.BaseRef, ChangedFiles: 1}, ParentSHA: in.ExpectedAnchor.ParentSHA, ObservedAt: stamp()}
	return f, a, receipt.ID, current
}

func coverageFinalize(t *testing.T, f *attemptFixture, a Attempt, id string, revision int64) {
	t.Helper()
	adjudicationComplete(t, f, a)
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "coverage-finalize", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(revision)})
	adjudicationReceipt(t, body, err, id, "finalized", revision+1)
}

func coverageFix(t *testing.T, f *attemptFixture, id, key string, revision, finding int64, previous string) {
	t.Helper()
	body, err := f.e.RecordFindingFix(t.Context(), f.scope(t, attemptProject, f.writer, false), key, id, FindingFixRequest{ExpectedRevision: adjudicationRevision(revision), Finding: finding, ExpectedSHA: &previous, FixSHA: strings.Repeat("f", 40), ProofNote: "Explicit focused proof"})
	adjudicationReceipt(t, body, err, id, "fix_recorded", revision+1)
}

func coverageRead(t *testing.T, f *attemptFixture, a Attempt, current SourcePlan, want bool) {
	t.Helper()
	before := adjudicationState(t, f, a)
	got, err := f.e.Coverage(t.Context(), attemptProject, current, "cli")
	if err != nil || got != want {
		t.Fatalf("coverage got %v want %v: %v", got, want, err)
	}
	if adjudicationState(t, f, a) != before {
		t.Fatal("coverage read changed evidence, journal, events or receipts")
	}
}

func TestCLICoverageCurrentProofAndFrozenCountsAcrossCorrections(t *testing.T) {
	f, a, id, current := coverageFixture(t, 2, false)
	coverageRead(t, f, a, current, false) // Active, even though the capture is complete.
	adjudicationDecide(t, f, id, "accept", 0, 0, "accepted")
	adjudicationDecide(t, f, id, "reject", 1, 1, "rejected")
	coverageRead(t, f, a, current, false) // No finalization.
	coverageFinalize(t, f, a, id, 2)
	coverageRead(t, f, a, current, false) // Accepted finding still has no fix reference.
	coverageFix(t, f, id, "fix-first", 3, 0, "")
	coverageRead(t, f, a, current, true)
	// A current rejection corrected to acceptance does not rewrite the original
	// accepted count, but does require its own current fix proof.
	adjudicationDecide(t, f, id, "correct-second", 4, 1, "accepted")
	coverageRead(t, f, a, current, false)
	coverageFix(t, f, id, "fix-second", 5, 1, "")
	coverageRead(t, f, a, current, true)
	body, err := f.e.RecordFindingFix(t.Context(), f.scope(t, attemptProject, f.writer, false), "correct-fix", id, FindingFixRequest{ExpectedRevision: adjudicationRevision(6), Finding: 0, ExpectedSHA: adjudicationSHA(strings.Repeat("f", 40)), FixSHA: strings.Repeat("e", 40), ProofNote: "Corrected immutable fix reference"})
	adjudicationReceipt(t, body, err, id, "fix_recorded", 7)
	coverageRead(t, f, a, current, true)
	state := adjudicationRead(t, f, id)
	if state.FrozenCounts == nil || *state.FrozenCounts != (FindingCounts{Found: 2, Accepted: 1}) {
		t.Fatal("coverage corrections altered frozen counts", state)
	}
	// Current actor authorization and current operator bindings are independent
	// action-boundary checks, not a rewrite of completed source evidence.
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
	coverageRead(t, f, a, current, true)
}

func TestCLICoverageZeroAndIncompleteEvidence(t *testing.T) {
	t.Run("zero", func(t *testing.T) {
		f, a, id, current := coverageFixture(t, 0, false)
		coverageRead(t, f, a, current, false)
		coverageFinalize(t, f, a, id, 0)
		coverageRead(t, f, a, current, true)
	})
	t.Run("partial", func(t *testing.T) {
		f, a, id, current := coverageFixture(t, 1, true)
		adjudicationDecide(t, f, id, "reject-partial", 0, 0, "rejected")
		adjudicationComplete(t, f, a)
		coverageComparison(t, f, a, current, ComparisonState{Pending: true})
	})
	t.Run("pending", func(t *testing.T) {
		f, a, id, current := coverageFixture(t, 1, false)
		adjudicationComplete(t, f, a)
		coverageRead(t, f, a, current, false)
		adjudicationDecide(t, f, id, "still-unfinalized", 0, 0, "rejected")
		coverageRead(t, f, a, current, false)
	})
}

func TestCLICoverageExactComparisonAndRepositoryRename(t *testing.T) {
	f, a, id, current := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	for _, test := range []struct {
		name   string
		change func(*SourcePlan)
		want   bool
		code   string
	}{
		{"current", nil, true, ""},
		{"rename", func(x *SourcePlan) { x.Repository.FullName = "renamed-owner/renamed-repository" }, true, ""},
		{"new-head", func(x *SourcePlan) { x.Pull.HeadSHA = strings.Repeat("9", 40) }, false, ""},
		{"new-base", func(x *SourcePlan) { x.Pull.BaseSHA = strings.Repeat("8", 40); x.ParentSHA = x.Pull.BaseSHA }, false, ""},
		{"new-parent-number", func(x *SourcePlan) { x.ParentPullNumber = 4 }, false, ""},
		{"new-base-ref", func(x *SourcePlan) { x.Pull.BaseRef = "other-base"; x.Repository.DefaultBranch = "other-base" }, false, ""},
		{"other-pull", func(x *SourcePlan) { x.Pull.Number = 4 }, false, ""},
		{"foreign-repository", func(x *SourcePlan) { x.Repository.ID = 802; x.Pull.RepositoryID = 802 }, false, "review_held"},
		{"invalid-parent", func(x *SourcePlan) { x.ParentSHA = strings.Repeat("7", 40) }, false, "validation_failed"},
		{"invalid-repository-binding", func(x *SourcePlan) { x.Pull.RepositoryID = 802 }, false, "validation_failed"},
		{"invalid-parent-number", func(x *SourcePlan) { x.ParentPullNumber = x.Pull.Number }, false, "validation_failed"},
		{"invalid-timestamp", func(x *SourcePlan) { x.ObservedAt = "not-a-time" }, false, "validation_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := current
			if test.change != nil {
				test.change(&input)
			}
			before := adjudicationState(t, f, a)
			got, err := f.e.Coverage(t.Context(), attemptProject, input, "cli")
			if test.code != "" {
				attemptCode(t, err, test.code)
				if got {
					t.Fatal("error returned coverage")
				}
			} else if err != nil || got != test.want {
				t.Fatal("incorrect comparison reuse", got, err)
			}
			if adjudicationState(t, f, a) != before {
				t.Fatal("comparison read changed retained state")
			}
		})
	}
	// The current foreign project has its own stable repository binding and may
	// not acquire this project's evidence even with identical PR/object strings.
	foreign := current
	foreign.Repository.ID = 802
	foreign.Pull.RepositoryID = 802
	got, err := f.e.Coverage(t.Context(), otherAttemptProject, foreign, "cli")
	if err != nil || got {
		t.Fatal("coverage leaked across projects", got, err)
	}
	// Historical completeness is not given a new local observation-age policy.
	old := current
	old.ObservedAt = "2020-01-01T00:00:00Z"
	coverageRead(t, f, a, old, true)
}

func TestCLICoverageImportedWorkspaceDoesNotCreateNativeEvidence(t *testing.T) {
	f := newAttemptFixture(t)
	if err := EnsureEvidenceSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if err := EnsureAdjudicationSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	pull := projectionPull(2)
	pull.Runs = []reviews.RunInput{{SourceRunID: "imported-complete-cli", Channel: "cli", HeadSHA: pull.HeadSHA, Reviewer: "Imported reviewer", Scope: "exact patch", Outcome: "completed", Attributable: true, Complete: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", Finalized: true, Findings: []reviews.Finding{}, CoverageLimits: []string{}}}
	hosted := pull.Runs[0]
	hosted.Channel, hosted.SourceRunID = "hosted", "imported-complete-hosted"
	pull.Runs = append(pull.Runs, hosted)
	zero := int64(0)
	request := reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 801, Source: "imported-history", ObservedAt: stamp(), Queue: []reviews.PullInput{pull}}
	mux := http.NewServeMux()
	reviews.Register(mux, f.store)
	r := httptest.NewRequest("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", strings.NewReader(encoded(request)))
	r.Header.Set("Authorization", "Bearer "+f.admin)
	r.Header.Set("Idempotency-Key", "synthetic-import")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatal("import fixture", w.Code, w.Body.String())
	}
	current := SourcePlan{Repository: reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, Pull: reviewprovider.Pull{Number: 2, RepositoryID: 801, State: "open", HeadSHA: pull.HeadSHA, BaseSHA: pull.BaseSHA, BaseRef: pull.BaseRef, ChangedFiles: 1}, ParentSHA: pull.BaseSHA, ObservedAt: stamp()}
	before := projectionDigest(t, f, "")
	for _, channel := range []string{"cli", "hosted"} {
		got, err := f.e.Comparison(t.Context(), attemptProject, current, channel)
		if err != nil || got != (ComparisonState{}) || projectionDigest(t, f, "") != before || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
			t.Fatal("import acquired native coverage or read changed state", channel, got, err)
		}
	}
}

func TestCLICoverageMalformedRetainedProofFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*ReviewEvidence, *ReviewAdjudication)
	}{
		{"anchor", func(e *ReviewEvidence, s *ReviewAdjudication) { e.Anchor.PatchSHA256 = "invalid" }},
		{"capture-count", func(e *ReviewEvidence, s *ReviewAdjudication) { e.Capture.Review.ReportedFindings++ }},
		{"capture-identity", func(e *ReviewEvidence, s *ReviewAdjudication) { e.AttemptID = "other-attempt" }},
		{"missing-finding", func(e *ReviewEvidence, s *ReviewAdjudication) { s.Findings = nil }},
		{"frozen-counts", func(e *ReviewEvidence, s *ReviewAdjudication) { s.FrozenCounts.Found++ }},
		{"invalid-reason", func(e *ReviewEvidence, s *ReviewAdjudication) { s.Findings[0].Reason = "" }},
		{"invalid-actor", func(e *ReviewEvidence, s *ReviewAdjudication) { s.Findings[0].ActorID = "bad actor" }},
		{"invalid-decision-time", func(e *ReviewEvidence, s *ReviewAdjudication) { s.Findings[0].DecidedAt = "not-a-time" }},
		{"invalid-fix", func(e *ReviewEvidence, s *ReviewAdjudication) { s.Findings[0].FixSHA = "short" }},
		{"invalid-proof", func(e *ReviewEvidence, s *ReviewAdjudication) { s.Findings[0].ProofNote = "" }},
		{"invalid-fix-time", func(e *ReviewEvidence, s *ReviewAdjudication) { s.Findings[0].FixedAt = "not-a-time" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, a, id, current := coverageFixture(t, 1, false)
			adjudicationDecide(t, f, id, "accepted", 0, 0, "accepted")
			coverageFix(t, f, id, "fixed", 1, 0, "")
			coverageFinalize(t, f, a, id, 2)
			evidence, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
			if err != nil {
				t.Fatal(err)
			}
			state := adjudicationRead(t, f, id)
			test.change(&evidence, &state)
			admissionSQL(t, f, `UPDATE review_execution_evidence SET body_json=? WHERE id=?`, encoded(evidence), id)
			admissionSQL(t, f, `UPDATE review_adjudication_state SET body_json=? WHERE evidence_id=?`, encoded(state), id)
			before := adjudicationState(t, f, a)
			got, err := f.e.Coverage(t.Context(), attemptProject, current, "cli")
			attemptCode(t, err, "storage_unavailable")
			if got || adjudicationState(t, f, a) != before {
				t.Fatal("malformed retained proof gave credit or changed state")
			}
		})
	}
}

func TestCLICoverageCorruptHistoryAndCancellation(t *testing.T) {
	f, a, id, current := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	// Duplicate only compact local journal rows, not captures or snapshots. The
	// invalidation triggers must refuse these malformed bodies without giving credit.
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		for i := 0; i < 100; i++ {
			operation, attempt, evidence := fmt.Sprintf("bounded-operation-%d", i), fmt.Sprintf("bounded-attempt-%d", i), fmt.Sprintf("bounded-evidence-%d", i)
			if _, err := tx.ExecContext(ctx, `INSERT INTO review_operations(id,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at,capability_fingerprint,policy_revision) SELECT ?,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,?,created_at,updated_at,capability_fingerprint,policy_revision FROM review_operations WHERE id=?`, operation, attempt, a.OperationID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO review_execution_attempts(id,operation_id,project_id,provider,account,state,revision,anchor_json,intent,external_id) SELECT ?,?,project_id,provider,account,state,revision,anchor_json,intent,external_id FROM review_execution_attempts WHERE id=?`, attempt, operation, a.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO review_execution_evidence(id,project_id,attempt_id,operation_id,body_json,fingerprint) VALUES(?,?,?,?,?,?)`, evidence, attemptProject, attempt, operation, "{}", "synthetic-bounded-body"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before := adjudicationState(t, f, a)
	got, err := f.e.Coverage(t.Context(), attemptProject, current, "cli")
	attemptCode(t, err, "storage_unavailable")
	if got || adjudicationState(t, f, a) != before {
		t.Fatal("corrupt history changed state or yielded coverage")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	started := time.Now()
	got, err = f.e.Coverage(ctx, attemptProject, current, "cli")
	if err == nil || got || time.Since(started) > time.Second {
		t.Fatal("cancelled predicate did not stop promptly", got, err)
	}
}

func coverageSeedOperations(t *testing.T, f *attemptFixture, a Attempt, count int, state string) {
	t.Helper()
	// Model retained journals whose completed intents have no capture, as the
	// startup backfill can encounter. Derive only pending from genuine absence;
	// never fabricate ready credit or replace this fixture's engine options.
	// Actual journal/capture/finalization growth is proved separately.
	// Keep each fixture transaction within the ordinary write budget under race.
	const batchSize = 64
	for start := 0; start < count; start += batchSize {
		err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			for i := start; i < min(start+batchSize, count); i++ {
				operation, attempt := fmt.Sprintf("history-operation-%d", i), fmt.Sprintf("history-attempt-%d", i)
				if _, err := tx.ExecContext(ctx, `INSERT INTO review_operations(id,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at,capability_fingerprint,policy_revision) SELECT ?,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,?,revision,reason,?,created_at,updated_at,capability_fingerprint,policy_revision FROM review_operations WHERE id=?`, operation, state, attempt, a.OperationID); err != nil {
					return err
				}
				if state == "completed" {
					if err := refreshComparisonProof(ctx, tx, attemptProject, operation); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestCLICoverageFailedSameHeadHistoryExcluded(t *testing.T) {
	f, a, id, current := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	// Missing attempts/captures in failed history must neither consume the
	// completed-operation budget nor get decoded as coverage candidates.
	coverageSeedOperations(t, f, a, 1024, "failed")
	coverageRead(t, f, a, current, true)
}

func TestCLICoverageCompletedMissingCaptureGrowth(t *testing.T) {
	for _, count := range []int{100, 102} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f, a, _, current := coverageFixture(t, 0, false)
			// Settle the original attempt to release its real reservation; its
			// unfinalized capture and older missing captures remain obligations.
			adjudicationComplete(t, f, a)
			comparisonMissingHistory(t, f, count, "cli", strings.Repeat("9", 40))
			coverageComparison(t, f, a, current, ComparisonState{Pending: true})
		})
	}
}

func TestCLICoverageCompletedOperationAttemptMismatch(t *testing.T) {
	f, a, id, current := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	admissionSQL(t, f, `UPDATE review_operations SET attempt_id='different-attempt' WHERE id=?`, a.OperationID)
	before := adjudicationState(t, f, a)
	got, err := f.e.Coverage(t.Context(), attemptProject, current, "cli")
	attemptCode(t, err, "storage_unavailable")
	if got || adjudicationState(t, f, a) != before {
		t.Fatal("mismatched retained attempt gave credit or changed state", got, err)
	}
}

// coverageNextAttempt creates a distinct explicit intent; it does not bypass any
// production reservation/admission checks or manufacture evidence rows.
func coverageNextAttempt(t *testing.T, f *attemptFixture, key, channel, head string) Attempt {
	t.Helper()
	body, err := f.e.Enqueue(t.Context(), f.scope(t, attemptProject, f.writer, false), key, Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: channel, HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		t.Fatal(err)
	}
	a := f.start(t, attemptProject, receipt)
	anchor := attemptAnchor(attemptProject, 2)
	anchor.CandidateSHA, anchor.ParentSHA = head, anchor.BaseSHA
	intent := ""
	if channel == "hosted" {
		intent = "explicit-hosted-intent"
	}
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, intent)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func coverageComparison(t *testing.T, f *attemptFixture, a Attempt, plan SourcePlan, want ComparisonState) {
	t.Helper()
	before := adjudicationState(t, f, a)
	got, err := f.e.Comparison(t.Context(), attemptProject, plan, "cli")
	if err != nil || got != want {
		t.Fatalf("comparison got %+v want %+v: %v", got, want, err)
	}
	coverageRead(t, f, a, plan, want.Covered && !want.Pending)
	if adjudicationState(t, f, a) != before {
		t.Fatal("comparison mutated retained evidence")
	}
}

func TestCLIComparisonOlderHeadPendingUntilClosed(t *testing.T) {
	f, a, id, current := coverageFixture(t, 1, false)
	adjudicationComplete(t, f, a)
	current.Pull.HeadSHA = strings.Repeat("9", 40)
	coverageComparison(t, f, a, current, ComparisonState{Pending: true})
	adjudicationDecide(t, f, id, "accept-old", 0, 0, "accepted")
	coverageComparison(t, f, a, current, ComparisonState{Pending: true})
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "finalize-old", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
	adjudicationReceipt(t, body, err, id, "finalized", 2)
	coverageComparison(t, f, a, current, ComparisonState{Pending: true})
	coverageFix(t, f, id, "fix-old", 2, 0, "")
	// Closing old obligations does not grant coverage for the new comparison.
	coverageComparison(t, f, a, current, ComparisonState{})
}

func TestCLIComparisonReadyRunCannotMaskPendingRun(t *testing.T) {
	f, a, id, current := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	source, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
	if err != nil {
		t.Fatal(err)
	}
	next := coverageNextAttempt(t, f, "another-explicit-intent", "cli", current.Pull.HeadSHA)
	capture := source.Capture
	capture.ExpectedAnchor = *next.Anchor
	receipt, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, next.ID, capture)
	if err != nil {
		t.Fatal(err)
	}
	adjudicationComplete(t, f, next)
	coverageComparison(t, f, a, current, ComparisonState{Covered: true, Pending: true})
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "close-second", receipt.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(0)})
	adjudicationReceipt(t, body, err, receipt.ID, "finalized", 1)
	coverageComparison(t, f, a, current, ComparisonState{Covered: true})
	// A completed intent with no retained capture also dominates ready credit.
	comparisonCompleteMissingCapture(t, f, "missing-after-ready", "cli", current.Pull.HeadSHA)
	coverageComparison(t, f, a, current, ComparisonState{Covered: true, Pending: true})
}

func TestComparisonGrowthUsesStateAndAnchorIndexes(t *testing.T) {
	f, a, id, current := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	coverageSeedOperations(t, f, a, 1536, "failed")
	comparisonCompleteMissingCapture(t, f, "indexed-hosted-missing", "hosted", current.Pull.HeadSHA)
	coverageRead(t, f, a, current, true)
	hostedComparisonRead(t, f, a, current, ComparisonState{Pending: true})
	assertComparisonGrowthQueryPlans(t, f, current)
	var obsolete int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='review_operations_cli_admission'`).Scan(&obsolete); err != nil || obsolete != 0 {
		t.Fatal("obsolete index retained", obsolete, err)
	}
}
