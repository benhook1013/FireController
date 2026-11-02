package reviewruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func sourceOperation(f *cliDriverFixture, number int64, channel string) reviewexec.Operation {
	return reviewexec.Operation{ProjectID: f.project, ConfigurationRevision: 1, PullNumber: number, Channel: channel, HeadSHA: f.reader.pulls[number].HeadSHA}
}
func sourceConfiguration(mode string, queue ...int64) reviewexec.Configuration {
	return reviewexec.Configuration{CapabilityID: "runtime-capability", Mode: mode, Queue: queue, Channels: []string{"cli", "hosted"}}
}

// Seed trusted hosted ledger evidence through the actual journal methods.
// It is a synthetic scope-proven zero-finding capture, not a provider claim.
func seedSourceHostedEvidence(t *testing.T, f *cliDriverFixture, finalize bool) string {
	t.Helper()
	ctx := t.Context()
	request := reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "hosted", HeadSHA: f.headSHA}
	body, err := f.engine.Enqueue(ctx, f.scope(t, f.writer, false), "source-hosted-request", request)
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		t.Fatal(err)
	}
	a, err := f.engine.StartAttempt(ctx, f.project, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := f.manager.Prepare(ctx, 901, a.ID, f.headSHA, f.baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	anchor := reviewexec.Anchor{RepositoryID: 901, Repository: "owner/repo", PullNumber: 2, CandidateSHA: f.headSHA, BaseSHA: f.baseSHA, ParentSHA: f.baseSHA, BaseRef: "main", MergeBaseSHA: workspace.MergeBaseSHA, PatchSHA256: workspace.PatchSHA256, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: f.headSHA})
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.engine.Admit(ctx, f.project, a.ID, a.Revision, anchor, intent)
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.engine.Advance(ctx, f.project, a.ID, a.Revision, "active", "source_fixture_posted", "101")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("d", 64)
	capture := reviewexec.ReviewEvidenceInput{CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1000, StartedAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), FinishedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpectedAnchor: anchor, Review: reviewprovider.ParsedReview{Outcome: "completed", Complete: true, Findings: []reviewprovider.ParsedFinding{}}, Hosted: &reviewexec.HostedEvidenceMetadata{PublicationID: "11111111-1111-1111-1111-111111111111", ProviderAttemptID: "22222222-2222-2222-2222-222222222222", TriggerCommentID: 101, ReviewIDs: []int64{201}, ObservedBaseSHA: f.baseSHA, Sources: []reviewexec.HostedFindingSource{}}}
	retained, err := f.engine.RecordReviewEvidence(ctx, f.project, a.ID, capture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.Advance(ctx, f.project, a.ID, a.Revision, "completed", "source_fixture_capture", ""); err != nil {
		t.Fatal(err)
	}
	if finalize {
		zero := int64(0)
		if _, err = f.engine.FinalizeReviewEvidence(ctx, f.scope(t, f.writer, false), "source-hosted-finalize", retained.ID, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero}); err != nil {
			t.Fatal(err)
		}
	}
	return retained.ID
}

func sourceState(t *testing.T, f *cliDriverFixture) [6]int {
	t.Helper()
	var result [6]int
	for i, table := range []string{"review_operations", "review_execution_transitions", "review_execution_evidence", "review_adjudication_changes", "events", "receipts"} {
		result[i] = cliCount(t, f.store, "SELECT count(*) FROM "+table)
	}
	return result
}
func sourceHoldReason(t *testing.T, err error, reason string) {
	t.Helper()
	assertCoreErrorCode(t, err, "review_held")
	var failure *core.Error
	if !errors.As(err, &failure) {
		t.Fatal(err)
	}
	details, ok := failure.Details.(map[string]string)
	if !ok || details["reason"] != reason {
		t.Fatal("wrong source hold", err)
	}
}

func TestResolveReviewSourceHostedDoesNotUseCLICreditOrObligations(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "covered"}[closed], func(t *testing.T) {
			f := newCLIDriverFixture(t)
			if closed {
				seedCLIDriverNativeCoverage(t, f, 2)
			} else {
				cliDriverCompletedFinding(t, f)
			}
			before := sourceState(t, f)
			plan, predecessors, err := resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, sourceConfiguration("independent", 2), sourceOperation(f, 2, "hosted"))
			if err != nil || plan.Pull.Number != 2 || len(predecessors) != 0 {
				t.Fatal("CLI evidence changed hosted source selection", plan, err)
			}
			if sourceState(t, f) != before {
				t.Fatal("source read changed durable state")
			}
		})
	}
}

func TestResolveReviewSourceHostedPendingAndCoverageAreSelectedChannelOnly(t *testing.T) {
	f := newCLIDriverFixture(t)
	id := seedSourceHostedEvidence(t, f, false)
	config := sourceConfiguration("independent", 2)
	before := sourceState(t, f)
	_, _, err := resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, config, sourceOperation(f, 2, "hosted"))
	assertCoreErrorCode(t, err, "review_evidence_pending")
	_, _, err = resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, config, sourceOperation(f, 2, "cli"))
	if err != nil {
		t.Fatal("hosted pending evidence blocked CLI", err)
	}
	if sourceState(t, f) != before {
		t.Fatal("source gate wrote while reading")
	}
	zero := int64(0)
	if _, err = f.engine.FinalizeReviewEvidence(t.Context(), f.scope(t, f.writer, false), "close-hosted-source", id, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero}); err != nil {
		t.Fatal(err)
	}
	before = sourceState(t, f)
	_, _, err = resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, config, sourceOperation(f, 2, "hosted"))
	sourceHoldReason(t, err, "target_already_covered")
	_, _, err = resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, config, sourceOperation(f, 2, "cli"))
	if err != nil || sourceState(t, f) != before {
		t.Fatal("hosted coverage blocked CLI or mutated state", err)
	}
}

func TestResolveReviewSourceHostedStackAndPhaseLocalReads(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			if channel == "cli" {
				seedCLIDriverNativeCoverage(t, f, 2)
			} else {
				seedSourceHostedEvidence(t, f, true)
			}
			p := f.reader.pulls[3]
			p.Merged, p.State = true, "closed"
			f.reader.pulls[3] = p
			p = f.reader.pulls[4]
			p.HeadSHA, p.BaseSHA, p.BaseRef = strings.Repeat("9", 40), f.headSHA, "parent-branch"
			f.reader.pulls[4] = p
			parent := f.reader.pulls[2]
			parent.HeadRef = "parent-branch"
			f.reader.pulls[2] = parent
			config := sourceConfiguration("stacked", 2, 3, 4)
			operation := sourceOperation(f, 4, "hosted")
			before := sourceState(t, f)
			reads := newSourceReads(f.reader)
			plan, predecessors, err := resolveReviewSource(t.Context(), f.engine, reads, 901, config, operation)
			if channel == "cli" {
				sourceHoldReason(t, err, "predecessor_unfinished")
				if plan.Pull.Number != 4 || predecessors != nil {
					t.Fatal("failed prefix lost target or retained partial dependencies")
				}
				return
			}
			if err != nil || len(predecessors) != 1 || predecessors[0].Pull.Number != 2 || plan.ParentPullNumber != 2 {
				t.Fatal("hosted stack scope/order changed", plan, predecessors, err)
			}
			// Recheck and repeated selection share only this phase's provider facts.
			if err := reviewexec.RecheckSource(t.Context(), reads, 901, config, operationRequest(operation), plan); err != nil {
				t.Fatal(err)
			}
			if _, _, err = resolveReviewSource(t.Context(), f.engine, reads, 901, config, operation); err != nil {
				t.Fatal(err)
			}
			if f.reader.repositoryCalls != 1 || len(f.reader.calls) != 2 {
				t.Fatal("unbounded source calls", f.reader.repositoryCalls, f.reader.calls)
			}
			for _, number := range []int64{2, 4} {
				if f.reader.calls[number] != 1 {
					t.Fatal("repeated reads within phase", f.reader.calls)
				}
			}
			// New phase must observe movement rather than reuse the previous cache.
			p = f.reader.pulls[4]
			p.HeadSHA = strings.Repeat("8", 40)
			f.reader.pulls[4] = p
			_, _, err = resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, config, operation)
			sourceHoldReason(t, err, "target_changed")
			if f.reader.repositoryCalls != 2 || f.reader.calls[4] != 2 || f.reader.calls[2] != 1 || f.reader.calls[3] != 0 {
				t.Fatal("new phase reused old facts or read unrelated targets", f.reader.repositoryCalls, f.reader.calls)
			}
			if sourceState(t, f) != before {
				t.Fatal("source selection changed durable state")
			}
		})
	}
}

type sourceFaultReader struct {
	*cliDriverReader
	failRepository, failPull bool
}

func (r *sourceFaultReader) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	value, err := r.cliDriverReader.Repository(ctx, id)
	if r.failRepository {
		r.failRepository = false
		return value, errors.New("synthetic incomplete repository observation")
	}
	return value, err
}
func (r *sourceFaultReader) Pull(ctx context.Context, repo string, number int64) (reviewprovider.Pull, error) {
	value, err := r.cliDriverReader.Pull(ctx, repo, number)
	if r.failPull {
		r.failPull = false
		return value, errors.New("synthetic incomplete pull observation")
	}
	return value, err
}

func TestSourceReadsDoNotCachePartialErrorsAndValidateBeforeReads(t *testing.T) {
	f := newCLIDriverFixture(t)
	reader := &sourceFaultReader{cliDriverReader: f.reader, failRepository: true, failPull: true}
	reads := newSourceReads(reader)
	operation := sourceOperation(f, 2, "hosted")
	config := sourceConfiguration("independent", 2)
	for _, channel := range []string{"", "Hosted", "manual"} {
		bad := operation
		bad.Channel = channel
		_, _, err := resolveReviewSource(t.Context(), f.engine, reads, 901, config, bad)
		assertCoreErrorCode(t, err, "validation_failed")
	}
	if reader.repositoryCalls != 0 || len(reader.calls) != 0 {
		t.Fatal("invalid channel contacted source")
	}
	for i := 0; i < 2; i++ {
		if _, _, err := resolveReviewSource(t.Context(), f.engine, reads, 901, config, operation); err == nil {
			t.Fatal("partial observation accepted")
		}
	}
	if _, _, err := resolveReviewSource(t.Context(), f.engine, reads, 901, config, operation); err != nil {
		t.Fatal("partial error poisoned later complete read", err)
	}
	if reader.repositoryCalls != 2 || reader.calls[2] != 2 {
		t.Fatal("errors cached or successes needlessly reread", reader.repositoryCalls, reader.calls)
	}
}
