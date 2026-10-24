package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// EquivalentComparison is a trusted pinned-Git proof, never a worker assertion.
type EquivalentComparison struct {
	OriginalBaseTree, OriginalHeadTree, CurrentBaseTree, CurrentHeadTree    string
	OriginalMergeBaseSHA, CurrentMergeBaseSHA, PatchSHA256, InventorySHA256 string
}

// ApplicabilityVerifier is registered by trusted operator composition. It may
// acquire pinned Git objects, but cannot request or credit another review.
type ApplicabilityVerifier interface {
	VerifyReviewApplicability(context.Context, Anchor, SourcePlan) (EquivalentComparison, error)
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

// Consider the newest native ready evidence independently for each channel.
// This is bounded repair of normal parent carries, not a historical search.
func (e *Engine) prepareReviewApplicability(ctx context.Context, project string, plan SourcePlan, capability string) []ReviewApplicability {
	e.sourceMu.Lock()
	verifier := e.applicabilityVerifiers[capability]
	e.sourceMu.Unlock()
	if verifier == nil || !validCoverageSource(plan) {
		return nil
	}
	var originals []ReviewApplicability
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, channel := range []string{"cli", "hosted"} {
			var evidenceID string
			err := tx.QueryRowContext(ctx, `SELECT p.evidence_id FROM review_comparison_proofs p JOIN review_execution_evidence e ON e.id=p.evidence_id WHERE p.project_id=? AND p.pull_number=? AND p.channel=? AND p.repository_id=? AND p.state='ready' ORDER BY e.rowid DESC LIMIT 1`, project, plan.Pull.Number, channel, plan.Repository.ID).Scan(&evidenceID)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			evidence, _, err := loadReviewAdjudication(ctx, tx, project, evidenceID)
			if err != nil {
				return err
			}
			var retained bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_applicability WHERE project_id=? AND evidence_id=? AND repository_id=? AND pull_number=? AND channel=? AND candidate_sha=? AND base_sha=? AND parent_sha=? AND parent_pull_number=? AND base_ref=?)`, project, evidenceID, plan.Repository.ID, plan.Pull.Number, channel, plan.Pull.HeadSHA, plan.Pull.BaseSHA, plan.ParentSHA, plan.ParentPullNumber, plan.Pull.BaseRef).Scan(&retained); err != nil {
				return err
			}
			if retained {
				continue
			}
			a := evidence.Anchor
			if a.CandidateSHA == plan.Pull.HeadSHA && a.BaseSHA == plan.Pull.BaseSHA && a.ParentSHA == plan.ParentSHA && a.ParentPullNumber == plan.ParentPullNumber && a.BaseRef == plan.Pull.BaseRef {
				continue
			}
			originals = append(originals, ReviewApplicability{EvidenceID: evidenceID, Channel: channel, Original: a})
		}
		return nil
	})
	if err != nil {
		return nil
	}
	var proofs []ReviewApplicability
	for _, p := range originals {
		trees, err := verifier.VerifyReviewApplicability(ctx, p.Original, plan)
		if err != nil {
			continue
		}
		p.Current = Anchor{RepositoryID: plan.Repository.ID, Repository: plan.Repository.FullName, PullNumber: plan.Pull.Number, CandidateSHA: plan.Pull.HeadSHA, BaseSHA: plan.Pull.BaseSHA, ParentSHA: plan.ParentSHA, ParentPullNumber: plan.ParentPullNumber, BaseRef: plan.Pull.BaseRef, MergeBaseSHA: trees.CurrentMergeBaseSHA, PatchSHA256: trees.PatchSHA256, ObservedAt: plan.ObservedAt}
		p.Trees, p.RecordedAt = trees, time.Now().UTC().Format(time.RFC3339Nano)
		if validApplicability(p) {
			proofs = append(proofs, p)
		}
	}
	return proofs
}

func (e *Engine) retainReviewApplicability(ctx context.Context, tx *sql.Tx, project string, plan SourcePlan, proofs []ReviewApplicability) error {
	for _, p := range proofs {
		if !validApplicability(p) || p.Current.RepositoryID != plan.Repository.ID || p.Current.PullNumber != plan.Pull.Number || p.Current.CandidateSHA != plan.Pull.HeadSHA || p.Current.BaseSHA != plan.Pull.BaseSHA || p.Current.ParentSHA != plan.ParentSHA || p.Current.ParentPullNumber != plan.ParentPullNumber || p.Current.BaseRef != plan.Pull.BaseRef {
			return adjudicationStorageError()
		}
		evidence, state, err := loadReviewAdjudication(ctx, tx, project, p.EvidenceID)
		if err != nil {
			return err
		}
		derived, err := deriveComparisonProof(ctx, tx, project, evidence.OperationID)
		if err != nil {
			return err
		}
		ready, err := coverageProof(ctx, tx, evidence, state, p.Channel)
		if err != nil {
			return err
		}
		// A concurrent correction may remove eligibility; it never grants proof.
		if !ready || derived.state != "ready" || derived.channel != p.Channel || !reflect.DeepEqual(evidence.Anchor, p.Original) {
			continue
		}
		b, err := json.Marshal(p)
		if err != nil {
			return err
		}
		id := applicabilityID(p)
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT body_json FROM review_applicability WHERE id=? AND project_id=?`, id, project).Scan(&existing)
		if err == nil {
			var old ReviewApplicability
			if strictjson.Decode([]byte(existing), &old) != nil || !validApplicability(old) || applicabilityID(old) != id {
				return adjudicationStorageError()
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO review_applicability(id,project_id,evidence_id,repository_id,pull_number,channel,candidate_sha,base_sha,parent_sha,parent_pull_number,base_ref,body_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, id, project, p.EvidenceID, p.Current.RepositoryID, p.Current.PullNumber, p.Channel, p.Current.CandidateSHA, p.Current.BaseSHA, p.Current.ParentSHA, p.Current.ParentPullNumber, p.Current.BaseRef, string(b))
		if err != nil {
			return err
		}
	}
	return nil
}

// ValidateReviewApplicability checks retained attestations without network/Git
// reads. Current readiness still joins the original live native proof state.
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

// RegisterApplicabilityVerifier binds one optional verifier to an existing
// capability before serving. It does not replace any source-reader interfaces.
func (e *Engine) RegisterApplicabilityVerifier(capability string, verifier ApplicabilityVerifier) error {
	if e == nil || verifier == nil {
		return invalid()
	}
	if _, ok := e.capabilities[capability]; !ok {
		return invalid()
	}
	e.sourceMu.Lock()
	defer e.sourceMu.Unlock()
	if e.applicabilityVerifiers == nil {
		e.applicabilityVerifiers = make(map[string]ApplicabilityVerifier)
	}
	if e.applicabilityVerifiers[capability] != nil {
		return conflict()
	}
	e.applicabilityVerifiers[capability] = verifier
	return nil
}
