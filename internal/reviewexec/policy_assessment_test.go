package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func assessmentPolicy(t *testing.T, f *attemptFixture, previous int64, required, paused []string) {
	t.Helper()
	policyConfigure(t, f, f.scope(t, attemptProject, f.admin, true), fmt.Sprintf("assessment-policy-%d", previous), PolicyConfiguration{ExpectedRevision: &previous, RequiredChannels: required, PausedChannels: paused})
}

func assessmentRead(t *testing.T, f *attemptFixture, a Attempt, id, state string, revision int64, channels ...PolicyAssessmentChannel) PolicyAssessment {
	t.Helper()
	before := adjudicationState(t, f, a) + policyState(t, f)
	got, err := f.e.ReadPolicyAssessment(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
	if err != nil || got.ProjectID != attemptProject || got.EvidenceID != id || got.Source != "captured_comparison" || got.State != state || got.PolicyRevision != revision || got.Anchor != *a.Anchor || got.Channels == nil || len(got.Channels) != len(channels) {
		t.Fatalf("aggregate assessment: %+v %v", got, err)
	}
	found := map[string]PolicyAssessmentChannel{}
	for _, channel := range got.Channels {
		if _, exists := found[channel.Channel]; exists {
			t.Fatal("duplicate assessment channel")
		}
		found[channel.Channel] = channel
	}
	for _, want := range channels {
		if found[want.Channel] != want {
			t.Fatalf("channel %s got %+v want %+v", want.Channel, found[want.Channel], want)
		}
	}
	at, err := time.Parse(time.RFC3339Nano, got.EvaluatedAt)
	if err != nil || at.IsZero() || got.EvaluatedAt != at.UTC().Format(time.RFC3339Nano) || time.Since(at) > time.Second {
		t.Fatal("assessment lacks current UTC evaluation time", got.EvaluatedAt)
	}
	if before != adjudicationState(t, f, a)+policyState(t, f) {
		t.Fatal("assessment mutated evidence, policy, journal, events or receipts")
	}
	return got
}

// Add independent hosted evidence through the real journal/capture APIs, keeping
// the supplied comparison exact; no provider observation is fabricated as credit.
func assessmentHosted(t *testing.T, f *attemptFixture, anchor Anchor) (Attempt, string) {
	t.Helper()
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "assessment-hosted", "hosted", 2, 1))
	anchor.ObservedAt = stamp()
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: anchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, intent)
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "synthetic-posted", "101")
	if err != nil {
		t.Fatal(err)
	}
	metadata, _, parsed := hostedEvidenceMetadataFixture()
	metadata.ObservedBaseSHA = anchor.ParentSHA
	metadata.Sources = []HostedFindingSource{}
	parsed.Findings = []reviewprovider.ParsedFinding{}
	parsed.ReportedFindings = 0
	hash := strings.Repeat("2", 64)
	in := ReviewEvidenceInput{CaptureID: "review_capture_" + hash[:32], CaptureSHA256: hash, CaptureBytes: 1024, StartedAt: stamp(), FinishedAt: stamp(), ExpectedAnchor: anchor, Review: parsed, Hosted: metadata}
	receipt, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	adjudicationComplete(t, f, a)
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "assessment-hosted-finalize", receipt.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(0)})
	adjudicationReceipt(t, body, err, receipt.ID, "finalized", 1)
	return a, receipt.ID
}

func TestPolicyAssessmentCombinesRequiredChannelsAndCurrentPolicy(t *testing.T) {
	f, a, id, _ := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	assessmentRead(t, f, a, id, "held", 1, PolicyAssessmentChannel{Channel: "cli", Covered: true}, PolicyAssessmentChannel{Channel: "hosted"})
	hosted, hostedID := assessmentHosted(t, f, *a.Anchor)
	assessmentRead(t, f, a, id, "sufficient", 1, PolicyAssessmentChannel{Channel: "cli", Covered: true}, PolicyAssessmentChannel{Channel: "hosted", Covered: true})
	assessmentRead(t, f, hosted, hostedID, "sufficient", 1, PolicyAssessmentChannel{Channel: "cli", Covered: true}, PolicyAssessmentChannel{Channel: "hosted", Covered: true})
	assessmentPolicy(t, f, 1, []string{"hosted", "cli"}, []string{"cli", "hosted"})
	assessmentRead(t, f, a, id, "sufficient", 2, PolicyAssessmentChannel{Channel: "cli", Covered: true}, PolicyAssessmentChannel{Channel: "hosted", Covered: true})
	// An enabled but no longer required channel's missing capture is irrelevant.
	coverageSeedOperations(t, f, hosted, 1, "completed")
	// This is deliberate retained missing-capture fixture setup, not a
	// corrupted ready operation. Re-derive its pending proof in the same write.
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE review_operations SET head_sha=? WHERE id='history-operation-0'`, strings.Repeat("9", 40)); err != nil {
			return err
		}
		return refreshComparisonProof(ctx, tx, attemptProject, "history-operation-0")
	}); err != nil {
		t.Fatal(err)
	}
	assessmentPolicy(t, f, 2, []string{"cli"}, []string{})
	assessmentRead(t, f, a, id, "sufficient", 3, PolicyAssessmentChannel{Channel: "cli", Covered: true})
	assessmentPolicy(t, f, 3, []string{"cli", "hosted"}, []string{})
	assessmentRead(t, f, a, id, "held", 4, PolicyAssessmentChannel{Channel: "cli", Covered: true}, PolicyAssessmentChannel{Channel: "hosted", Covered: true, Pending: true})
}

func TestPolicyAssessmentUsesEffectiveCorrectionsAndFrozenSourceCounts(t *testing.T) {
	f, a, id, _ := coverageFixture(t, 2, false)
	assessmentPolicy(t, f, 1, []string{"cli"}, []string{})
	adjudicationComplete(t, f, a)
	assessmentRead(t, f, a, id, "held", 2, PolicyAssessmentChannel{Channel: "cli", Pending: true})
	adjudicationDecide(t, f, id, "assessment-accept", 0, 0, "accepted")
	adjudicationDecide(t, f, id, "assessment-reject", 1, 1, "rejected")
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "assessment-finalize", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(2)})
	adjudicationReceipt(t, body, err, id, "finalized", 3)
	assessmentRead(t, f, a, id, "held", 2, PolicyAssessmentChannel{Channel: "cli", Pending: true})
	coverageFix(t, f, id, "assessment-fix-first", 3, 0, "")
	assessmentRead(t, f, a, id, "sufficient", 2, PolicyAssessmentChannel{Channel: "cli", Covered: true})
	adjudicationDecide(t, f, id, "assessment-late-accept", 4, 1, "accepted")
	assessmentRead(t, f, a, id, "held", 2, PolicyAssessmentChannel{Channel: "cli", Pending: true})
	coverageFix(t, f, id, "assessment-fix-second", 5, 1, "")
	assessmentRead(t, f, a, id, "sufficient", 2, PolicyAssessmentChannel{Channel: "cli", Covered: true})
	if got := adjudicationRead(t, f, id); got.FrozenCounts == nil || *got.FrozenCounts != (FindingCounts{Found: 2, Accepted: 1}) {
		t.Fatal("effective correction rewrote frozen source counts", got)
	}
}

func TestPolicyAssessmentDoesNotMixComparisonsOrCreditIncompleteCapture(t *testing.T) {
	t.Run("different hosted comparison", func(t *testing.T) {
		f, a, id, _ := coverageFixture(t, 0, false)
		coverageFinalize(t, f, a, id, 0)
		other := *a.Anchor
		other.BaseSHA = strings.Repeat("9", 40)
		other.ParentSHA = other.BaseSHA
		hosted, hostedID := assessmentHosted(t, f, other)
		assessmentRead(t, f, a, id, "held", 1, PolicyAssessmentChannel{Channel: "cli", Covered: true}, PolicyAssessmentChannel{Channel: "hosted"})
		assessmentRead(t, f, hosted, hostedID, "held", 1, PolicyAssessmentChannel{Channel: "cli"}, PolicyAssessmentChannel{Channel: "hosted", Covered: true})
	})
	t.Run("incomplete capture", func(t *testing.T) {
		f, a, id, _ := coverageFixture(t, 1, true)
		adjudicationComplete(t, f, a)
		assessmentPolicy(t, f, 1, []string{"cli"}, []string{})
		adjudicationDecide(t, f, id, "assessment-partial-reject", 0, 0, "rejected")
		assessmentRead(t, f, a, id, "held", 2, PolicyAssessmentChannel{Channel: "cli", Pending: true})
	})
}

func TestPolicyAssessmentMissingPolicyDoesNotAllocateDefaults(t *testing.T) {
	f, a, id, _ := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	admissionSQL(t, f, `DELETE FROM review_policy_config WHERE project_id=?`, attemptProject)
	assessmentRead(t, f, a, id, "unconfigured", 0)
	if attemptCount(t, f.store, `SELECT count(*) FROM review_policy_config WHERE project_id=?`, attemptProject) != 0 {
		t.Fatal("read installed implicit policy")
	}
}

func TestPolicyAssessmentImportedHistoryCannotSupplyRequiredChannel(t *testing.T) {
	f, a, id, _ := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	reviews.Register(mux, f.store)
	f.handler = mux
	pull := projectionPull(2)
	pull.Runs = []reviews.RunInput{{SourceRunID: "imported-final-hosted", Channel: "hosted", HeadSHA: a.Anchor.CandidateSHA, Reviewer: "Imported reviewer", Scope: "Historical exact comparison", Outcome: "completed", Attributable: true, Complete: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", Finalized: true, CoverageLimits: []string{}, Findings: []reviews.Finding{}}}
	response := f.request(http.MethodPost, "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "assessment-history", encoded(reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: a.Anchor.RepositoryID, Source: "imported-history", ObservedAt: stamp(), Queue: []reviews.PullInput{pull}, Retired: []reviews.RetiredPull{}}))
	if response.Code != http.StatusCreated {
		t.Fatal("historical observation fixture", response.Code, response.Body.String())
	}
	before := projectionDigest(t, f, "")
	assessmentRead(t, f, a, id, "held", 1, PolicyAssessmentChannel{Channel: "cli", Covered: true}, PolicyAssessmentChannel{Channel: "hosted"})
	if before != projectionDigest(t, f, "") || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 1 {
		t.Fatal("aggregate read rewrote historical observations or manufactured native evidence")
	}
}

func TestPolicyAssessmentRechecksReadAuthorizationAndProjectIsolation(t *testing.T) {
	for _, name := range []string{"foreign-project", "changed-project", "revoked", "grant-removed"} {
		t.Run(name, func(t *testing.T) {
			f, a, id, _ := coverageFixture(t, 0, false)
			coverageFinalize(t, f, a, id, 0)
			scope := evidenceReadScope(t, f, attemptProject, f.reader)
			code := "forbidden"
			switch name {
			case "foreign-project":
				scope = evidenceReadScope(t, f, otherAttemptProject, f.other)
				code = "not_found"
			case "changed-project":
				scope.ProjectID = otherAttemptProject
			case "revoked":
				admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-reader-cred'`)
				code = "unauthorized"
			case "grant-removed":
				admissionSQL(t, f, `UPDATE grants SET can_read=0 WHERE credential_id='exec-reader-cred'`)
			}
			before := adjudicationState(t, f, a) + policyState(t, f)
			got, err := f.e.ReadPolicyAssessment(t.Context(), scope, id)
			attemptCode(t, err, code)
			if !reflect.DeepEqual(got, PolicyAssessment{}) || before != adjudicationState(t, f, a)+policyState(t, f) {
				t.Fatal("denied read exposed assessment or changed durable state", got)
			}
		})
	}
}

func TestPolicyAssessmentRejectsMalformedMissingOrCorruptTarget(t *testing.T) {
	for _, name := range []string{"invalid-id", "missing-id", "corrupt-source", "wrong-target-channel", "wrong-target-anchor", "empty-required-policy", "foreign-repository"} {
		t.Run(name, func(t *testing.T) {
			f, a, id, _ := coverageFixture(t, 0, false)
			coverageFinalize(t, f, a, id, 0)
			code := "storage_unavailable"
			switch name {
			case "invalid-id":
				id = "bad/id"
				code = "validation_failed"
			case "missing-id":
				id = "review_evidence_" + strings.Repeat("0", 32)
				code = "not_found"
			case "corrupt-source":
				admissionSQL(t, f, `UPDATE review_execution_evidence SET body_json='{}' WHERE id=?`, id)
			case "wrong-target-channel":
				admissionSQL(t, f, `UPDATE review_operations SET channel='hosted' WHERE id=?`, a.OperationID)
			case "wrong-target-anchor":
				retained, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
				if err != nil {
					t.Fatal(err)
				}
				retained.Anchor.CandidateSHA = strings.Repeat("9", 40)
				raw, err := json.Marshal(retained)
				if err != nil {
					t.Fatal(err)
				}
				admissionSQL(t, f, `UPDATE review_execution_evidence SET body_json=? WHERE id=?`, string(raw), id)
			case "empty-required-policy":
				admissionSQL(t, f, `UPDATE review_policy_config SET body_json='{"required_channels":[],"paused_channels":[],"max_review_files":0}' WHERE project_id=?`, attemptProject)
			case "foreign-repository":
				admissionSQL(t, f, `UPDATE projects SET github_repository_id=999 WHERE id=?`, attemptProject)
				code = "review_held"
			}
			before := adjudicationState(t, f, a) + policyState(t, f)
			got, err := f.e.ReadPolicyAssessment(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
			attemptCode(t, err, code)
			if !reflect.DeepEqual(got, PolicyAssessment{}) || before != adjudicationState(t, f, a)+policyState(t, f) {
				t.Fatal("invalid target yielded credit or changed state", got)
			}
		})
	}
}

func TestPolicyAssessmentPendingHistoryCorruptionAndCancellation(t *testing.T) {
	f, a, id, _ := coverageFixture(t, 0, false)
	coverageSeedOperations(t, f, a, 100, "completed")
	assessmentRead(t, f, a, id, "held", 1, PolicyAssessmentChannel{Channel: "cli", Pending: true}, PolicyAssessmentChannel{Channel: "hosted"})
	// Directly completing only the operation leaves its actual attempt active.
	// Invalidation must refuse this corruption rather than pass cached facts.
	admissionSQL(t, f, `UPDATE review_operations SET state='completed' WHERE id=?`, a.OperationID)
	before := adjudicationState(t, f, a) + policyState(t, f)
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	got, err := f.e.ReadPolicyAssessment(t.Context(), scope, id)
	attemptCode(t, err, "storage_unavailable")
	if !reflect.DeepEqual(got, PolicyAssessment{}) || before != adjudicationState(t, f, a)+policyState(t, f) {
		t.Fatal("corrupt journal refusal created credit or state", got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	started := time.Now()
	got, err = f.e.ReadPolicyAssessment(ctx, scope, id)
	if err == nil || got.State == "sufficient" || time.Since(started) > time.Second || before != adjudicationState(t, f, a)+policyState(t, f) {
		t.Fatal("cancelled assessment did not return promptly without writes", got, err)
	}
}
