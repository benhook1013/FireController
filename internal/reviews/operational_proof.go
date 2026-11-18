package reviews

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/benhook1013/FireController/internal/core"
)

// OperationalProof attests an external remediation without identifying a code commit.
type OperationalProof struct {
	Digest     string `json:"digest"`
	Note       string `json:"note"`
	ActorID    string `json:"actor_id"`
	RecordedAt string `json:"recorded_at"`
}

// ValidOperationalProof checks the durable proof's shape and note integrity.
func ValidOperationalProof(p *OperationalProof) bool {
	if p == nil {
		return false
	}
	digest := sha256.Sum256([]byte(p.Note))
	return p.Digest == hex.EncodeToString(digest[:]) && nativeAdjudicationText(p.Note, 2048, true) && nativeAdjudicationText(p.ActorID, 128, true) && nativeAdjudicationTime(p.RecordedAt)
}

func decodeOperationalProof(raw, disposition, sha, note, at string) (*OperationalProof, error) {
	var p *OperationalProof
	if err := strictStoredJSON(raw, &p); err != nil {
		return nil, err
	}
	if p != nil && (disposition != "accepted" || sha != "" || note != "" || at != "" || !ValidOperationalProof(p)) {
		return nil, core.Fail("storage_unavailable", "Stored operational proof is invalid.", 500)
	}
	return p, nil
}

// ValidateOperationalProofHistory checks retained proof bodies without a provider read.
func ValidateOperationalProofHistory(ctx context.Context, q HistoryQuery) error {
	rows, err := q.QueryContext(ctx, `SELECT CASE WHEN length(CAST(operational_proof_json AS BLOB))<=16384 THEN operational_proof_json END,disposition,fix_sha,proof_note,resolved_at FROM review_finding_versions WHERE operational_proof_json != 'null'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw, disposition, sha, note, at string
		if err := rows.Scan(&raw, &disposition, &sha, &note, &at); err != nil {
			return err
		}
		proof, err := decodeOperationalProof(raw, disposition, sha, note, at)
		if err != nil {
			return err
		}
		if proof == nil {
			return core.Fail("storage_unavailable", "Stored operational proof must use its canonical absent value.", 500)
		}
	}
	return rows.Err()
}
