package reviewexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// EquivalentComparison is a trusted pinned-Git proof, never a worker assertion.
type EquivalentComparison struct {
	OriginalBaseTree, OriginalHeadTree, CurrentBaseTree, CurrentHeadTree    string
	OriginalMergeBaseSHA, CurrentMergeBaseSHA, PatchSHA256, InventorySHA256 string
}

type ReviewApplicability struct {
	EvidenceID string               `json:"evidence_id"`
	Channel    string               `json:"channel"`
	Original   Anchor               `json:"original"`
	Current    Anchor               `json:"current"`
	Trees      EquivalentComparison `json:"trees"`
	RecordedAt string               `json:"recorded_at"`
}

func ApplicabilitySchemaSQL() []string {
	return []string{
		`CREATE TABLE review_applicability(id TEXT PRIMARY KEY NOT NULL,project_id TEXT NOT NULL REFERENCES projects(id),evidence_id TEXT NOT NULL REFERENCES review_execution_evidence(id),repository_id INTEGER NOT NULL,pull_number INTEGER NOT NULL,channel TEXT NOT NULL,candidate_sha TEXT NOT NULL,base_sha TEXT NOT NULL,parent_sha TEXT NOT NULL,parent_pull_number INTEGER NOT NULL,base_ref TEXT NOT NULL,body_json TEXT NOT NULL);`,
		`CREATE INDEX review_applicability_target ON review_applicability(project_id,pull_number,channel,repository_id,candidate_sha,base_sha,parent_sha,parent_pull_number,base_ref,evidence_id);`,
		`CREATE TRIGGER review_applicability_retain BEFORE DELETE ON review_applicability BEGIN SELECT RAISE(ABORT,'retain review applicability'); END;`,
		`CREATE TRIGGER review_applicability_immutable BEFORE UPDATE ON review_applicability BEGIN SELECT RAISE(ABORT,'retain original review applicability'); END;`,
	}
}

func validApplicability(p ReviewApplicability) bool {
	t := p.Trees
	at, err := time.Parse(time.RFC3339Nano, p.RecordedAt)
	return validEvidenceAnchor(p.Original) && validEvidenceAnchor(p.Current) && identifier.MatchString(p.EvidenceID) && validChannel(p.Channel) && err == nil && !at.IsZero() &&
		p.Original.RepositoryID == p.Current.RepositoryID && p.Original.PullNumber == p.Current.PullNumber &&
		sha.MatchString(t.OriginalBaseTree) && t.OriginalBaseTree == t.CurrentBaseTree && sha.MatchString(t.OriginalHeadTree) && t.OriginalHeadTree == t.CurrentHeadTree &&
		t.OriginalMergeBaseSHA == p.Original.MergeBaseSHA && t.CurrentMergeBaseSHA == p.Current.MergeBaseSHA &&
		t.PatchSHA256 == p.Original.PatchSHA256 && t.PatchSHA256 == p.Current.PatchSHA256 && hash.MatchString(t.InventorySHA256)
}

func applicabilityID(p ReviewApplicability) string {
	// Source observation times may advance without changing either comparison.
	p.Original.ObservedAt, p.Current.ObservedAt, p.RecordedAt = "", "", ""
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return "review_applicability_" + hex.EncodeToString(sum[:16])
}

// ValidateReviewApplicability checks retained attestations without network/Git
// reads. These historical records do not gate current PR review progress.
func ValidateReviewApplicability(ctx context.Context, q EvidenceQuery) error {
	var malformed bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_applicability WHERE id IS NULL OR id='')`).Scan(&malformed); err != nil {
		return err
	}
	if malformed {
		return adjudicationStorageError()
	}
	last := ""
	for {
		rows, err := q.QueryContext(ctx, `SELECT id,project_id,evidence_id,repository_id,pull_number,channel,candidate_sha,base_sha,parent_sha,parent_pull_number,base_ref,body_json FROM review_applicability WHERE id>? ORDER BY id LIMIT 64`, last)
		if err != nil {
			return err
		}
		type record struct {
			id, project, evidence, channel, candidate, base, parent, ref, body string
			repository, number, parentNumber                                   int64
		}
		var records []record
		for rows.Next() {
			var r record
			if err = rows.Scan(&r.id, &r.project, &r.evidence, &r.repository, &r.number, &r.channel, &r.candidate, &r.base, &r.parent, &r.parentNumber, &r.ref, &r.body); err != nil {
				break
			}
			records = append(records, r)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if len(records) == 0 {
			return nil
		}
		for _, r := range records {
			var p ReviewApplicability
			if len(r.body) > 8192 || strictjson.Decode([]byte(r.body), &p) != nil || encoded(p) != r.body || !validApplicability(p) || r.id != applicabilityID(p) || r.evidence != p.EvidenceID || r.repository != p.Current.RepositoryID || r.number != p.Current.PullNumber || r.channel != p.Channel || r.candidate != p.Current.CandidateSHA || r.base != p.Current.BaseSHA || r.parent != p.Current.ParentSHA || r.parentNumber != p.Current.ParentPullNumber || r.ref != p.Current.BaseRef {
				return adjudicationStorageError()
			}
			evidence, _, err := loadReviewAdjudication(ctx, q, r.project, p.EvidenceID)
			if err != nil {
				return err
			}
			derived, err := deriveComparisonProof(ctx, q, r.project, evidence.OperationID)
			if err != nil {
				return err
			}
			if derived.channel != p.Channel || evidence.Anchor != p.Original {
				return adjudicationStorageError()
			}
			last = r.id
		}
	}
}
