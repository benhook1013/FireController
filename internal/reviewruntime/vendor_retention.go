package reviewruntime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

const vendorSessionRetention = 7 * 24 * time.Hour

// vendorSessionMetadata is the versioned CLI's private git.json identity.
// The policy is deliberately inactive: there is no scheduler or deletion path.
// Qualification preserves controller evidence but does not claim byte-equivalent
// recovery of vendor-only summaries/history. Destruction requires its own approval.
type vendorSessionMetadata struct {
	Head                   string `json:"head"`
	BaseBranch             string `json:"baseBranch"`
	BaseCommitID           string `json:"baseCommitId"`
	ComparisonBaseCommitID string `json:"comparisonBaseCommitId"`
	WorkingDirectory       string `json:"workingDirectory"`
}

// qualifyVendorSession is a bounded, read-only policy check for one enumerated
// session. It never invokes the provider, parses findings, or changes review state.
func (d *CLIDriver) qualifyVendorSession(ctx context.Context, metadata, marker []byte, now time.Time) (bool, error) {
	if d == nil || len(metadata) > 8192 || len(marker) > 1024 || now.IsZero() {
		return false, nil
	}
	var m vendorSessionMetadata
	var complete struct {
		Version            int    `json:"version"`
		State              string `json:"state"`
		HasGitMetadata     bool   `json:"hasGitMetadata"`
		HasIncrementalDiff bool   `json:"hasIncrementalDiff"`
	}
	if json.Unmarshal(metadata, &m) != nil || json.Unmarshal(marker, &complete) != nil || complete.Version != 2 || complete.State != "complete" || !complete.HasGitMetadata || !complete.HasIncrementalDiff {
		return false, nil
	}
	id := strings.TrimPrefix(m.BaseBranch, "refs/firecontroller/review-base/")
	if id == m.BaseBranch || !validIdentifier(id) || !strings.HasPrefix(id, "review_attempt_") || path.Base(m.WorkingDirectory) != id {
		return false, nil
	}
	allowed, err := d.engine.WorkspaceDisposable(ctx, d.capability.ID, id, now)
	if err != nil || !allowed {
		return false, err
	}
	var body, completedAt string
	err = d.store.DB().QueryRowContext(ctx, `SELECT e.body_json,o.updated_at FROM review_execution_evidence e JOIN review_execution_attempts a ON a.id=e.attempt_id AND a.project_id=e.project_id JOIN review_operations o ON o.id=a.operation_id AND o.project_id=a.project_id WHERE a.id=? AND a.state='completed' AND o.state='completed' AND o.channel='cli' AND o.attempt_id=a.id`, id).Scan(&body, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	completed, timeErr := time.Parse(time.RFC3339Nano, completedAt)
	if timeErr != nil || completed.After(now.Add(-vendorSessionRetention)) {
		return false, nil
	}
	if len(body) > 2<<20 {
		return false, nil
	}
	var e reviewexec.ReviewEvidence
	if json.Unmarshal([]byte(body), &e) != nil {
		return false, nil
	}
	_, ref, err := d.captures.Read(ctx, id)
	if err != nil {
		return false, err
	}
	return vendorRetentionEvidence(m, id, e, ref, now), nil
}

func vendorRetentionEvidence(m vendorSessionMetadata, id string, e reviewexec.ReviewEvidence, ref CaptureReference, now time.Time) bool {
	c := e.Capture
	finished, err := time.Parse(time.RFC3339Nano, c.FinishedAt)
	recorded, recordErr := time.Parse(time.RFC3339Nano, e.RecordedAt)
	started, startErr := time.Parse(time.RFC3339Nano, c.StartedAt)
	return err == nil && recordErr == nil && startErr == nil && !finished.Before(started) && !recorded.Before(finished) && !recorded.After(now.Add(-vendorSessionRetention)) &&
		e.AttemptID == id && c.Hosted == nil && c.Review.Complete && c.Review.Outcome == "completed" &&
		ref.ID != "" && ref.SHA256 != "" && ref.Bytes > 0 && ref.ID == c.CaptureID && ref.SHA256 == c.CaptureSHA256 && ref.Bytes == c.CaptureBytes &&
		m.Head != "" && m.Head == e.Anchor.CandidateSHA && m.BaseCommitID != "" && m.BaseCommitID == e.Anchor.MergeBaseSHA && m.ComparisonBaseCommitID == e.Anchor.MergeBaseSHA &&
		e.Anchor == c.ExpectedAnchor
}
