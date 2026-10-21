package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

// ExternalHostedReader adds an explicitly operator-bound bot identity to the
// read-only source adapter. It has no posting, allowance or execution methods.
type ExternalHostedReader interface {
	TargetReader
	ObserveHosted(context.Context, string, int64) (reviewprovider.HostedPublicationSnapshot, error)
	TrustedHostedActor() reviewprovider.Author
}

func externalHostedUnproven() error {
	return &reviewprovider.Error{Code: "external_hosted_identity_unproven", Message: "Some external hosted inventory is incomplete, unsupported, over the current collection budget or has unresolved native identity."}
}

func collectExternalHosted(ctx context.Context, target lifecycleTarget, repository string, earlier error) ([]reviewprovider.ExternalHostedRun, error) {
	reader, ok := target.reader.(ExternalHostedReader)
	if !ok {
		return nil, nil
	}
	var limited *reviewprovider.Error
	if errors.As(earlier, &limited) && (limited.RetryAfterSeconds > 0 || limited.Code == "provider_rate_limited") {
		return nil, earlier
	}
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	snapshot, err := reader.ObserveHosted(readCtx, repository, target.number)
	if err != nil {
		return nil, err
	}
	if !snapshot.Complete {
		return nil, externalHostedUnproven()
	}
	return reviewprovider.DiscoverExternalHosted(snapshot, reader.TrustedHostedActor())
}

func (e *Engine) externalHostedInputs(ctx context.Context, tx *sql.Tx, target lifecycleTarget, external []reviewprovider.ExternalHostedRun) ([]reviews.RunInput, bool, error) {
	cutoff, err := reviews.InitializeExternalHostedFrontier(ctx, tx, target.project, target.repository, target.number)
	if err != nil {
		return nil, false, err
	}
	var floor time.Time
	if cutoff != "" {
		floor, err = time.Parse(time.RFC3339Nano, cutoff)
		if err != nil {
			return nil, false, refreshStorageError()
		}
	}
	runs := []reviews.RunInput{}
	held := false
	for _, run := range external {
		if !run.FinishedAt.After(floor) {
			continue
		}
		if !run.Inventory.Complete || run.FinishedAt.IsZero() || len(run.Inventory.Findings) > 100 {
			held = true
			continue
		}

		// Existing exact source identities are immutable. Skip them before spending
		// the new-result budget, so bounded ticks progress beyond their first batch.
		var owner int64
		var channel, head string
		err := tx.QueryRowContext(ctx, `SELECT pull_number,channel,head_sha FROM review_run_versions INDEXED BY review_run_versions_external_source WHERE project_id=? AND source_run_id=? LIMIT 1`, target.project, run.SourceID).Scan(&owner, &channel, &head)
		if err == nil {
			if owner != target.number || channel != "hosted" || head != run.HeadSHA {
				held = true
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, false, err
		}
		captures, uncertain, err := externalNativeCaptures(ctx, tx, target, run)
		if err != nil {
			return nil, false, err
		}
		if uncertain {
			held = true
			continue
		}
		owned, ambiguous := externalCapturedIdentity(captures, run)
		if owned {
			continue
		}
		if ambiguous {
			held = true
			continue
		}
		owned, ambiguous, err = externalHostedNativeBinding(ctx, tx, target, run)
		if err != nil {
			return nil, false, err
		}
		if owned {
			continue
		}
		if ambiguous {
			held = true
			continue
		}

		mapped := reviews.RunInput{SourceRunID: run.SourceID, Channel: "hosted", HeadSHA: run.HeadSHA, Reviewer: "CodeRabbit", Scope: "external hosted publication", Outcome: "completed", Attributable: true, Complete: true, StartedAt: run.FinishedAt.UTC().Format(time.RFC3339Nano), FinishedAt: run.FinishedAt.UTC().Format(time.RFC3339Nano), Found: int64(len(run.Inventory.Findings)), Finalized: false, CoverageLimits: []string{"Observed external hosted inventory only; no native review coverage, adjudication or round credit.", "Both timestamps identify provider publication; execution start time and runtime were not observed."}, Findings: []reviews.Finding{}}
		for _, finding := range run.Inventory.Findings {
			mapped.Findings = append(mapped.Findings, reviews.Finding{SourceFindingID: finding.SourceKey, Title: finding.Title, Detail: finding.Detail, Severity: finding.Severity, Disposition: "unadjudicated"})
		}
		runs = append(runs, mapped)
	}
	return runs, held, nil
}

// A marker is merely a locator. Native ownership requires the real project,
// operation, attempt, posted comment and captured provider identities. An
// unresolved real posting window is deferred individually, never a blanket
// exclusion of later manually issued reviews on the same PR or head.
func externalHostedNativeBinding(ctx context.Context, tx *sql.Tx, target lifecycleTarget, run reviewprovider.ExternalHostedRun) (owned, held bool, err error) {
	if run.NativeOperationID == "" || run.TriggerCommentID <= 0 {
		return false, false, nil
	}
	o, err := readOperationBase(ctx, tx, target.project, run.NativeOperationID)
	if err != nil {
		var failure *core.Error
		if errors.As(err, &failure) && failure.Code == "not_found" {
			return false, false, nil
		}
		return false, false, err
	}
	if o.ProjectID != target.project || o.PullNumber != target.number || o.Channel != "hosted" || o.HeadSHA != run.HeadSHA || o.AttemptID == "" {
		return false, false, nil
	}
	a, err := readAttempt(ctx, tx, target.project, o.AttemptID)
	if err != nil {
		return false, true, nil
	}
	if a.OperationID != o.ID || a.ProjectID != target.project || a.Anchor == nil || a.Anchor.RepositoryID != target.repository || a.Anchor.PullNumber != target.number || a.Anchor.CandidateSHA != run.HeadSHA {
		return false, false, nil
	}
	intent, intentErr := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: o.ID, AttemptID: a.ID, HeadSHA: o.HeadSHA})
	if intentErr != nil || a.Intent != intent {
		return false, true, nil
	}
	if a.ExternalID == "" {
		return false, true, nil
	}
	if a.ExternalID != strconv.FormatInt(run.TriggerCommentID, 10) {
		return false, false, nil
	}
	if a.State == "posting" || a.State == "active" || a.State == "uncertain" {
		return true, false, nil
	}
	evidenceID, err := readHostedRecoveryEvidence(ctx, tx, a, o)
	if err != nil || evidenceID == "" {
		return false, true, nil
	}
	evidence, _, err := loadReviewAdjudication(ctx, tx, target.project, evidenceID)
	if err != nil || evidence.Capture.Hosted == nil {
		return false, true, nil
	}
	h := evidence.Capture.Hosted
	want, got := slices.Clone(run.ReviewIDs), slices.Clone(h.ReviewIDs)
	slices.Sort(want)
	slices.Sort(got)
	if h.TriggerCommentID != run.TriggerCommentID || !slices.Equal(want, got) {
		return false, true, nil
	}
	if publication := run.Inventory.Publication.PublicationID; publication != "" {
		matched := h.PublicationID == publication && h.ProviderAttemptID == run.Inventory.Publication.ProviderAttemptID
		return matched, !matched, nil
	}
	if h.SubmittedReview != nil {
		matched := h.SubmittedReview.ProviderRunID == run.ProviderRunID
		return matched, !matched, nil
	}
	if h.StandingSummary != nil {
		matched := h.StandingSummary.ProviderRunID == run.ProviderRunID
		return matched, !matched, nil
	}
	return false, true, nil
}

// Look up exact provider UUIDs through immutable capture expression indexes. This is independent
// of current marker windows: a bot can edit an old standing comment after a new
// unrelated command, without making the old provider run external.
type externalNativeCapture struct {
	head   string
	hosted *HostedEvidenceMetadata
}

func externalNativeCaptures(ctx context.Context, tx *sql.Tx, target lifecycleTarget, run reviewprovider.ExternalHostedRun) ([]externalNativeCapture, bool, error) {
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	index, expression, identity := "review_evidence_hosted_publication", "json_extract(body_json,'$.capture.hosted.publication_id')", run.Inventory.Publication.PublicationID
	if identity == "" {
		index, expression, identity = "review_evidence_hosted_run", "coalesce(json_extract(body_json,'$.capture.hosted.submitted_review.provider_run_id'),json_extract(body_json,'$.capture.hosted.standing_summary.provider_run_id'))", run.ProviderRunID
	}
	if identity == "" {
		return nil, true, nil
	}
	rows, err := tx.QueryContext(readCtx, `SELECT id,operation_id,attempt_id FROM review_execution_evidence INDEXED BY `+index+` WHERE project_id=? AND `+expression+`=? LIMIT 3`, target.project, identity)
	if err != nil {
		if readCtx.Err() != nil && ctx.Err() == nil {
			return nil, true, nil
		}
		return nil, false, err
	}
	type binding struct{ id, operation, attempt string }
	bindings := []binding{}
	for rows.Next() {
		var b binding
		if err = rows.Scan(&b.id, &b.operation, &b.attempt); err != nil {
			rows.Close()
			return nil, false, err
		}
		bindings = append(bindings, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		if readCtx.Err() != nil && ctx.Err() == nil {
			return nil, true, nil
		}
		return nil, false, err
	}
	if len(bindings) > 1 {
		return nil, true, nil
	}
	captures := []externalNativeCapture{}
	for _, b := range bindings {
		if readCtx.Err() != nil {
			return nil, true, nil
		}
		o, err := readOperationBase(readCtx, tx, target.project, b.operation)
		if err != nil {
			return nil, true, nil
		}
		a, err := readAttempt(readCtx, tx, target.project, b.attempt)
		if err != nil {
			return nil, true, nil
		}
		if o.Channel != "hosted" || o.PullNumber != target.number || a.Anchor == nil || a.Anchor.RepositoryID != target.repository || a.Anchor.PullNumber != target.number {
			return nil, true, nil
		}
		id, err := readHostedRecoveryEvidence(readCtx, tx, a, o)
		if err != nil || id != b.id {
			return nil, true, nil
		}
		evidence, _, err := loadReviewAdjudication(readCtx, tx, target.project, id)
		if err != nil || evidence.Capture.Hosted == nil {
			return nil, true, nil
		}
		captures = append(captures, externalNativeCapture{head: a.Anchor.CandidateSHA, hosted: evidence.Capture.Hosted})
	}
	return captures, false, nil
}
func externalCapturedIdentity(captures []externalNativeCapture, run reviewprovider.ExternalHostedRun) (owned, held bool) {
	for _, capture := range captures {
		h := capture.hosted
		uuid := run.Inventory.Publication.PublicationID != "" && h.PublicationID == run.Inventory.Publication.PublicationID
		if run.ProviderRunID != "" {
			uuid = uuid || h.SubmittedReview != nil && h.SubmittedReview.ProviderRunID == run.ProviderRunID || h.StandingSummary != nil && h.StandingSummary.ProviderRunID == run.ProviderRunID
		}
		overlap := false
		for _, id := range run.ReviewIDs {
			if slices.Contains(h.ReviewIDs, id) {
				overlap = true
				break
			}
		}
		if !uuid && !overlap {
			continue
		}
		want, got := slices.Clone(run.ReviewIDs), slices.Clone(h.ReviewIDs)
		slices.Sort(want)
		slices.Sort(got)
		exact := uuid && capture.head == run.HeadSHA && slices.Equal(want, got)
		if run.Inventory.Publication.PublicationID != "" {
			exact = exact && h.ProviderAttemptID == run.Inventory.Publication.ProviderAttemptID
		}
		if exact {
			return true, false
		}
		return false, true
	}
	return false, false
}

// Each candidate is independently bounded/validated. A rejected source does not
// rollback lifecycle metadata or discard another complete independent group.
// Savepoints also protect against later validation changes in the publisher.
func publishExternalHostedInputs(ctx context.Context, tx *sql.Tx, target lifecycleTarget, runs []reviews.RunInput) (bool, error) {
	held, appended := false, 0
	for len(runs) > 0 && appended < 100 {
		batch := []reviews.RunInput{}
		findings := 0
		for len(batch) < len(runs) && len(batch) < 100-appended {
			candidate := runs[len(batch)]
			if findings+len(candidate.Findings) > 1000 {
				break
			}
			encoded, err := json.Marshal(append(batch, candidate))
			if err != nil || len(encoded) > 256<<10 {
				break
			}
			batch = append(batch, candidate)
			findings += len(candidate.Findings)
		}
		if len(batch) == 0 {
			held = true
			runs = runs[1:]
			continue
		}
		changed, unsupported, err := publishExternalHostedBatch(ctx, tx, target, batch)
		if err != nil {
			return held, err
		}
		if !unsupported {
			if changed {
				appended += len(batch)
			}
		} else {
			held = true
			// Invalid source facts cannot prevent a separate complete group in the same
			// bounded batch from being retained. Most polls use one additive snapshot.
			for _, candidate := range batch {
				changed, _, err := publishExternalHostedBatch(ctx, tx, target, []reviews.RunInput{candidate})
				if err != nil {
					return held, err
				}
				if changed {
					appended++
				}
			}
		}
		runs = runs[len(batch):]
	}
	return held || len(runs) > 0, nil
}
func publishExternalHostedBatch(ctx context.Context, tx *sql.Tx, target lifecycleTarget, runs []reviews.RunInput) (changed, unsupported bool, err error) {
	if _, err = tx.ExecContext(ctx, `SAVEPOINT external_hosted_candidate`); err != nil {
		return false, false, err
	}
	_, changed, err = reviews.PublishExternalHosted(ctx, tx, core.Scope{ProjectID: target.project, PrincipalID: "service-hosted-observer"}, target.repository, target.number, runs)
	if err != nil {
		var failure *core.Error
		if !errors.As(err, &failure) || (failure.Code != "validation_failed" && failure.Code != "invalid_request" && failure.Code != "capacity_exceeded" && failure.Code != "observation_conflict") {
			return false, false, err
		}
		if _, err = tx.ExecContext(ctx, `ROLLBACK TO external_hosted_candidate`); err != nil {
			return false, false, err
		}
		unsupported = true
		changed = false
	}
	_, err = tx.ExecContext(ctx, `RELEASE external_hosted_candidate`)
	return changed, unsupported, err
}
