package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// Validate both directions: every recovery annotation has one immutable archive,
// and every archive binds exactly the current evidence and unchanged raw source.
func validateCLIEvidenceRecovery(ctx context.Context, db sqlReader) error {
	var missing int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM review_execution_evidence e LEFT JOIN review_cli_evidence_recovery r ON r.evidence_id=e.id WHERE json_extract(e.body_json,'$.recovery') IS NOT NULL AND r.evidence_id IS NULL`).Scan(&missing); err != nil || missing != 0 {
		return invalid()
	}
	rows, err := db.QueryContext(ctx, `SELECT r.evidence_id,r.project_id,CASE WHEN length(CAST(r.original_body_json AS BLOB))<=2105344 THEN r.original_body_json END,CASE WHEN length(CAST(r.original_fingerprint AS BLOB))=64 THEN r.original_fingerprint END,CASE WHEN length(CAST(r.recovered_fingerprint AS BLOB))=64 THEN r.recovered_fingerprint END,CASE WHEN length(CAST(r.recovered_at AS BLOB))<=64 THEN r.recovered_at END,CASE WHEN length(CAST(e.body_json AS BLOB))<=2105344 THEN e.body_json END,e.fingerprint,e.project_id FROM review_cli_evidence_recovery r LEFT JOIN review_execution_evidence e ON e.id=r.evidence_id`)
	if err != nil {
		return invalid()
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > maxArtifacts {
			return invalid()
		}
		if checkContext(ctx) != nil {
			return unavailable()
		}
		var id, project, body, prior, current, at, nextBody, nextFingerprint, nextProject string
		if rows.Scan(&id, &project, &body, &prior, &current, &at, &nextBody, &nextFingerprint, &nextProject) != nil {
			return invalid()
		}
		var old, next reviewexec.ReviewEvidence
		if strictjson.Decode([]byte(body), &old) != nil || strictjson.Decode([]byte(nextBody), &next) != nil {
			return invalid()
		}
		b, _ := json.Marshal(old)
		n, _ := json.Marshal(next)
		if string(b) != body || string(n) != nextBody || old.ID != id || next.ID != id || old.ProjectID != project || next.ProjectID != project || nextProject != project || old.Recovery != nil || next.Recovery == nil {
			return invalid()
		}
		if _, err := time.Parse(time.RFC3339Nano, at); err != nil {
			return invalid()
		}
		if next.Recovery.Reason != "cli_capture_reparsed" || next.Recovery.PriorFingerprint != prior || next.Recovery.RecoveredAt != at || current != nextFingerprint || !digest.MatchString(prior) || !digest.MatchString(current) {
			return invalid()
		}
		oldCapture, _ := json.Marshal(old.Capture)
		newCapture, _ := json.Marshal(next.Capture)
		a := sha256.Sum256(oldCapture)
		z := sha256.Sum256(newCapture)
		if hex.EncodeToString(a[:]) != prior || hex.EncodeToString(z[:]) != current || reviewexec.ValidateRetainedReviewCapture(old.Capture) != nil || reviewexec.ValidateRetainedReviewCapture(next.Capture) != nil {
			return invalid()
		}
		r := old.Capture.Review
		if old.Capture.Hosted != nil || next.Capture.Hosted != nil || r.Outcome != "uncertain" || r.Complete || r.ReportedFindings != 0 || len(r.Findings) != 0 || len(r.ReviewedFiles) != 0 || len(r.CoverageLimits) != 0 || !next.Capture.Review.Complete || next.Capture.Review.Outcome != "completed" {
			return invalid()
		}
		old.Capture.Review = next.Capture.Review
		old.Recovery = next.Recovery
		if !reflect.DeepEqual(old, next) {
			return invalid()
		}
	}
	if rows.Err() != nil {
		return invalid()
	}
	return nil
}
