package reviews

import (
	"context"
	"database/sql"
	"errors"
	"reflect"

	"github.com/benhook1013/FireController/internal/core"
)

// RecoverNativeCLIProjection replaces only an empty uncertain CLI projection with
// the verified interpretation of its same retained source. The owning evidence
// ledger must authenticate the original capture, archive its prior projection,
// and call this helper in that same transaction. No historical version is edited.
// Missing or changed current projections fail closed; this is not an import API.
func RecoverNativeCLIProjection(ctx context.Context, tx *sql.Tx, scope core.Scope, number int64, before, after RunInput) (Receipt, error) {
	conflict := func() (Receipt, error) {
		return Receipt{}, core.Fail("observation_conflict", "Only an unchanged empty native CLI capture projection can be recovered.", 409)
	}
	var p NativePublication
	p.Pull.Number = number
	var metadata string
	err := tx.QueryRowContext(ctx, `SELECT p.title,p.state,p.head_sha,p.base_ref,p.base_sha,p.ci_state,p.mergeability,p.complete,p.metadata_json FROM review_workspace w JOIN review_pulls p ON p.project_id=w.project_id AND p.snapshot_id=w.snapshot_id WHERE w.project_id=? AND p.number=?`, scope.ProjectID, number).Scan(&p.Pull.Title, &p.Pull.State, &p.Pull.HeadSHA, &p.Pull.BaseRef, &p.Pull.BaseSHA, &p.Pull.CIState, &p.Pull.Mergeability, &p.Pull.Complete, &metadata)
	if err != nil {
		return Receipt{}, err
	}
	m, err := ReadPullMetadata(metadata, p.Pull.State)
	if err != nil {
		return Receipt{}, err
	}
	p.Pull.Diff, p.Pull.IsDraft, p.Pull.MergedAt = m.Diff, m.IsDraft, m.MergedAt
	p.Run = &after
	if before.SourceRunID == "" || before.Channel != "cli" || before.Outcome != "uncertain" || before.Complete || before.Attributable || before.Finalized || before.Found != 0 || before.Accepted != 0 || before.Routed != 0 || len(before.Findings) != 0 {
		return conflict()
	}
	if after.SourceRunID != before.SourceRunID || after.Channel != before.Channel || after.HeadSHA != before.HeadSHA || after.Reviewer != before.Reviewer || after.Scope != before.Scope || after.StartedAt != before.StartedAt || after.FinishedAt != before.FinishedAt || !after.Complete || !after.Attributable || after.Outcome != "completed" || after.Finalized || after.Accepted != 0 || after.Routed != 0 || after.Found != int64(len(after.Findings)) {
		return conflict()
	}
	for _, f := range after.Findings {
		if f.Disposition != "unadjudicated" || f.DecisionReason != "" || f.Actor != "" || f.DecidedAt != "" || f.FixSHA != "" || f.ProofNote != "" || f.ResolvedAt != "" || f.Route != nil || f.OperationalProof != nil {
			return conflict()
		}
	}
	var snapshot string
	if err := tx.QueryRowContext(ctx, `SELECT snapshot_id FROM review_workspace WHERE project_id=?`, scope.ProjectID).Scan(&snapshot); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return conflict()
		}
		return Receipt{}, err
	}
	detail, err := readRunDetail(ctx, tx, snapshotMeta{ProjectID: scope.ProjectID, SnapshotID: snapshot}, runOpaqueID(scope.ProjectID, nativeSource, before.SourceRunID))
	if err != nil {
		return Receipt{}, err
	}
	if detail.Run.PullNumber != p.Pull.Number || detail.Run.Source != nativeSource || detail.Run.Fixed != 0 {
		return conflict()
	}
	actual := RunInput{SourceRunID: detail.Run.SourceRunID, Channel: detail.Run.Channel, HeadSHA: detail.Run.HeadSHA, Reviewer: detail.Run.Reviewer, Scope: detail.Run.Scope, Outcome: detail.Run.Outcome, Attributable: detail.Run.Attributable, Complete: detail.Run.Complete, StartedAt: detail.Run.StartedAt, FinishedAt: detail.Run.FinishedAt, Found: detail.Run.Found, Accepted: detail.Run.Accepted, Routed: detail.Run.Routed, Finalized: detail.Run.Finalized, CoverageLimits: detail.Run.CoverageLimits, Findings: []Finding{}}
	for _, f := range detail.Findings {
		actual.Findings = append(actual.Findings, f.Finding)
	}
	if before.Findings == nil {
		before.Findings = []Finding{}
	}
	if before.CoverageLimits == nil {
		before.CoverageLimits = []string{}
	}
	if actual.CoverageLimits == nil {
		actual.CoverageLimits = []string{}
	}
	if !reflect.DeepEqual(actual, before) {
		return conflict()
	}
	return publishNative(ctx, tx, scope, p, &before)
}
