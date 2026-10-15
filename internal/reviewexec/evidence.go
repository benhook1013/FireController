package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

const evidenceInputLimit = 2 << 20

// ReviewEvidenceInput is trusted parsed capture metadata, never a raw upload.
type ReviewEvidenceInput struct {
	CaptureID      string                      `json:"capture_id"`
	CaptureSHA256  string                      `json:"capture_sha256"`
	CaptureBytes   int64                       `json:"capture_bytes"`
	StartedAt      string                      `json:"started_at"`
	FinishedAt     string                      `json:"finished_at"`
	Review         reviewprovider.ParsedReview `json:"review"`
	ExpectedAnchor Anchor                      `json:"expected_anchor"`
	Hosted         *HostedEvidenceMetadata     `json:"hosted,omitempty"`
}

type ReviewEvidence struct {
	ID                    string              `json:"id"`
	ProjectID             string              `json:"project_id"`
	AttemptID             string              `json:"attempt_id"`
	OperationID           string              `json:"operation_id"`
	CapabilityFingerprint string              `json:"capability_fingerprint"`
	Anchor                Anchor              `json:"anchor"`
	Capture               ReviewEvidenceInput `json:"capture"`
	RecordedAt            string              `json:"recorded_at"`
}

func evidenceChannel(in ReviewEvidenceInput) string {
	if in.Hosted != nil {
		return "hosted"
	}
	return "cli"
}

func validEvidencePath(s string) bool {
	if s == "" || !utf8.ValidString(s) || strings.HasPrefix(s, "/") || strings.ContainsAny(s, "\\:") {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func validEvidenceAnchor(a Anchor) bool {
	t, err := time.Parse(time.RFC3339Nano, a.ObservedAt)
	return err == nil && !t.IsZero() && a.RepositoryID > 0 && repository.MatchString(a.Repository) && a.PullNumber > 0 && a.PullNumber <= 2147483647 && a.ParentPullNumber >= 0 && a.ParentPullNumber <= 2147483647 && a.BaseRef != "" && safeText(a.BaseRef, 256) && sha.MatchString(a.CandidateSHA) && sha.MatchString(a.BaseSHA) && sha.MatchString(a.ParentSHA) && sha.MatchString(a.MergeBaseSHA) && hash.MatchString(a.PatchSHA256)
}

// validateReviewEvidence checks immutable structure, never current wall-clock
// freshness. Retained captures remain valid after a host clock correction.
func validateReviewEvidence(in ReviewEvidenceInput) error {
	limit := int64(24 << 20)
	if in.Hosted != nil {
		limit = 48 << 20
	}
	if !hash.MatchString(in.CaptureSHA256) || in.CaptureID != "review_capture_"+in.CaptureSHA256[:32] || in.CaptureBytes < 1 || in.CaptureBytes > limit || !validEvidenceAnchor(in.ExpectedAnchor) {
		return invalid()
	}
	start, err := time.Parse(time.RFC3339Nano, in.StartedAt)
	end, endErr := time.Parse(time.RFC3339Nano, in.FinishedAt)
	if err != nil || endErr != nil || start.IsZero() || end.IsZero() || end.Before(start) {
		return invalid()
	}
	if in.Hosted != nil && in.Hosted.StandingSummary != nil {
		updated, err := time.Parse(time.RFC3339Nano, in.Hosted.StandingSummary.UpdatedAt)
		if err != nil || !updated.After(start) || updated.After(end) {
			return invalid()
		}
	}
	r := in.Review
	switch r.Outcome {
	case "completed", "failed", "cancelled", "rate_limited", "uncertain":
	default:
		return invalid()
	}
	if r.Complete != (r.Outcome == "completed") || len(r.Findings) > 100 || len(r.ReviewedFiles) > 10000 || r.ReportedFindings < 0 || r.Complete && (r.ReportedFindings != int64(len(r.Findings)) || in.Hosted == nil && len(r.ReviewedFiles) == 0) {
		return invalid()
	}
	seen := make(map[string]bool, len(r.ReviewedFiles))
	for _, path := range r.ReviewedFiles {
		if !validEvidencePath(path) || r.Complete && seen[path] {
			return invalid()
		}
		seen[path] = true
	}
	for _, f := range r.Findings {
		if !safeText(f.Title, 500) || strings.TrimSpace(f.Title) == "" || !safeText(f.Detail, 16<<10) || f.Path != "" && !validEvidencePath(f.Path) || f.StartLine < 0 || f.EndLine < 0 || f.StartLine != 0 && f.EndLine != 0 && f.EndLine < f.StartLine {
			return invalid()
		}
		switch f.Severity {
		case "info", "low", "medium", "high", "critical", "unknown":
		default:
			return invalid()
		}
	}
	if in.Hosted != nil {
		return validateHostedEvidenceMetadata(in.Hosted, in.ExpectedAnchor, r)
	}
	return nil
}

// RecordReviewEvidence retains one immutable capture without settling or crediting
// its attempt. Only trusted in-process capture integration may call this method.
func (e *Engine) RecordReviewEvidence(ctx context.Context, project, attemptID string, input ReviewEvidenceInput) (Receipt, error) {
	return e.recordReviewEvidenceAt(ctx, project, attemptID, input, time.Now())
}

func (e *Engine) recordReviewEvidenceAt(ctx context.Context, project, attemptID string, input ReviewEvidenceInput, now time.Time) (Receipt, error) {
	if !identifier.MatchString(project) || !identifier.MatchString(attemptID) {
		return Receipt{}, invalid()
	}
	// Validate before JSON encoding, which otherwise replaces invalid UTF-8.
	if err := validateReviewEvidence(input); err != nil {
		return Receipt{}, err
	}
	canonical, err := json.Marshal(input)
	if err != nil || len(canonical) > evidenceInputLimit {
		return Receipt{}, invalid()
	}
	var owned ReviewEvidenceInput
	if json.Unmarshal(canonical, &owned) != nil {
		return Receipt{}, invalid()
	}
	digest := sha256.Sum256(canonical)
	fingerprint := hex.EncodeToString(digest[:])
	var receipt Receipt
	err = e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, err := readAttempt(ctx, tx, project, attemptID)
		if err != nil {
			return err
		}
		o, err := readOperation(ctx, tx, project, a.OperationID)
		if err != nil {
			return err
		}
		var id, prior string
		err = tx.QueryRowContext(ctx, `SELECT id,fingerprint FROM review_execution_evidence WHERE project_id=? AND attempt_id=?`, project, attemptID).Scan(&id, &prior)
		if err == nil {
			if prior != fingerprint {
				return conflict()
			}
			receipt = Receipt{ID: id, State: "captured", Revision: 1}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// Only a genuinely new capture is subject to the admission clock. Exact
		// immutable replay above retains its original receipt after rollback.
		finished, _ := time.Parse(time.RFC3339Nano, owned.FinishedAt)
		if finished.After(now.Add(5 * time.Second)) {
			return invalid()
		}
		if !validChannel(o.Channel) || a.State != "active" && a.State != "uncertain" || a.Anchor == nil || !validEvidenceAnchor(*a.Anchor) || *a.Anchor != owned.ExpectedAnchor || a.Anchor.PullNumber != o.PullNumber || a.Anchor.CandidateSHA != o.HeadSHA || !hash.MatchString(o.CapabilityFingerprint) {
			return conflict()
		}
		if o.Channel == "cli" && owned.Hosted != nil || o.Channel == "hosted" && validateHostedEvidenceBinding(a, o, owned) != nil {
			return conflict()
		}
		var repositoryID int64
		if err = tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repositoryID); err != nil {
			return err
		}
		if repositoryID != a.Anchor.RepositoryID {
			return conflict()
		}
		record := ReviewEvidence{ID: core.NewID("review_evidence"), ProjectID: project, AttemptID: a.ID, OperationID: o.ID, CapabilityFingerprint: o.CapabilityFingerprint, Anchor: *a.Anchor, Capture: owned, RecordedAt: stamp()}
		body, err := json.Marshal(record)
		if err != nil {
			return invalid()
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_execution_evidence(id,project_id,attempt_id,operation_id,body_json,fingerprint) VALUES(?,?,?,?,?,?)`, record.ID, project, a.ID, o.ID, string(body), fingerprint); err != nil {
			return err
		}
		if err = core.AppendEvent(ctx, tx, core.Scope{PrincipalID: o.ActorID, ProjectID: project, WorkerID: o.WorkerID}, "review.captured", record.ID, 1, struct {
			AttemptID   string `json:"attempt_id"`
			OperationID string `json:"operation_id"`
		}{a.ID, o.ID}); err != nil {
			return err
		}
		receipt = Receipt{ID: record.ID, State: "captured", Revision: 1}
		return nil
	})
	if err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

// ReadReviewEvidence explicitly reads full retained evidence with current access.
func (e *Engine) ReadReviewEvidence(ctx context.Context, scope core.Scope, id string) (ReviewEvidence, error) {
	if !identifier.MatchString(id) {
		return ReviewEvidence{}, invalid()
	}
	var result ReviewEvidence
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var body string
		err := tx.QueryRowContext(ctx, `SELECT body_json FROM review_execution_evidence WHERE project_id=? AND id=?`, scope.ProjectID, id).Scan(&body)
		if errors.Is(err, sql.ErrNoRows) {
			return core.Fail("not_found", "The review capture is unavailable in this project.", 404)
		}
		if err != nil {
			return err
		}
		if len(body) > evidenceInputLimit+8192 || json.Unmarshal([]byte(body), &result) != nil || validateReviewEvidence(result.Capture) != nil {
			return core.Fail("storage_unavailable", "Stored review capture is unavailable.", 500)
		}
		return nil
	})
	if err != nil {
		return ReviewEvidence{}, err
	}
	return result, nil
}
