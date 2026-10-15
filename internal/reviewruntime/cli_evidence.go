package reviewruntime

import (
	"context"
	"slices"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

// CapturedCLI describes retained execution evidence, not adjudication, policy
// sufficiency or merge readiness. Routine status need not expose finding text.
type CapturedCLI struct {
	Receipt  reviewexec.Receipt `json:"receipt"`
	Outcome  string             `json:"outcome"`
	Complete bool               `json:"complete"`
	Reason   string             `json:"reason"`
}

// RetainCLI connects a private process capture to its immutable admission.
// Captures are durably saved before interpretation; malformed, incomplete or
// source-mismatched results remain inspectable without completed coverage.
// This is a trusted in-process boundary, never a client evidence-upload API.
func RetainCLI(ctx context.Context, captures *CaptureStore, engine *reviewexec.Engine, manager *reviewworkspace.Manager, attempt reviewexec.Attempt, workspace reviewworkspace.Workspace, capture reviewprocess.Capture, started, finished time.Time) (CapturedCLI, error) {
	if captures == nil || engine == nil || manager == nil || attempt.Anchor == nil {
		return CapturedCLI{}, core.Fail("validation_failed", "Use the admitted attempt and its owned review workspace.", 400)
	}
	reference, err := captures.Save(ctx, attempt.ID, CaptureRecord{Capture: capture, StartedAt: started, FinishedAt: finished})
	if err != nil {
		return CapturedCLI{}, err
	}
	retained, readReference, err := captures.Read(ctx, attempt.ID)
	if err != nil {
		return CapturedCLI{}, err
	}
	if readReference != reference {
		return CapturedCLI{}, captureError("capture_unavailable")
	}
	capture = retained.Capture
	review, parseErr := reviewprovider.ParseCLIReview(capture.Stdout, capture.ExitCode, reviewprovider.CLIContext{BaseBranch: workspace.ComparisonRef, WorkingDirectory: workspace.Directory})
	reason := "capture_retained"
	if parseErr != nil {
		review = reviewprovider.CLIReview{Outcome: "uncertain", Findings: []reviewprovider.CLIFinding{}, ReviewedFiles: []string{}}
		reason = "capture_invalid"
	}
	if !capture.Started {
		review.Outcome, review.Complete, reason = "failed", false, "process_not_started"
	}
	anchor := *attempt.Anchor
	sourceMatches := anchor.CandidateSHA == workspace.CandidateSHA && anchor.BaseSHA == workspace.BaseSHA && anchor.MergeBaseSHA == workspace.MergeBaseSHA && anchor.PatchSHA256 == workspace.PatchSHA256 && anchor.Repository == workspace.RepositoryCanonical
	if !sourceMatches {
		review.Outcome, review.Complete, reason = "uncertain", false, "source_changed"
	} else {
		paths, sourceErr := manager.ChangedFiles(ctx, workspace)
		if sourceErr != nil {
			review.Outcome, review.Complete, reason = "uncertain", false, "source_unavailable"
		} else if review.Complete {
			reviewed := append([]string(nil), review.ReviewedFiles...)
			slices.Sort(reviewed)
			if len(paths) == 0 || !slices.Equal(paths, reviewed) {
				review.Outcome, review.Complete, reason = "uncertain", false, "coverage_incomplete"
			}
		}
	}
	input := reviewexec.CLIEvidenceInput{CaptureID: reference.ID, CaptureSHA256: reference.SHA256, CaptureBytes: reference.Bytes, StartedAt: retained.StartedAt.Format(time.RFC3339Nano), FinishedAt: retained.FinishedAt.Format(time.RFC3339Nano), ExpectedAnchor: anchor, Review: review}
	receipt, err := engine.RecordCLIEvidence(ctx, attempt.ProjectID, attempt.ID, input)
	if err != nil {
		return CapturedCLI{}, err
	}
	return CapturedCLI{Receipt: receipt, Outcome: review.Outcome, Complete: review.Complete, Reason: reason}, nil
}
