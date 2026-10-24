package reviews

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"math"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/publicsite"
)

func validNativeHostedAlias(run RunInput, external string) bool {
	return run.Channel == "hosted" && run.Complete && run.Attributable && run.Outcome == "completed" && run.HeadSHA != "" && validSHA(run.HeadSHA) && externalHostedIdentity.MatchString(external)
}

// matchingExternalHostedRun performs a read-only preflight of this observer's
// exact current identity. Other producers with the same external ID are separate.
func matchingExternalHostedRun(ctx context.Context, tx *sql.Tx, project string, number int64, head, external string) (string, bool, error) {
	id := runOpaqueID(project, externalHostedSource, external)
	var owner, sourceRepository, currentRepository int64
	var source, sourceID, channel, retainedHead, outcome string
	var complete, attributable bool
	err := tx.QueryRowContext(ctx, `SELECT v.pull_number,v.source,v.source_run_id,v.channel,v.head_sha,v.complete,v.attributable,v.outcome,s.repository_id,p.github_repository_id
		FROM review_current_runs c JOIN review_run_memberships m ON m.project_id=c.project_id AND m.id=c.id AND m.start_revision=c.start_revision
		JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision
		JOIN review_snapshots s ON s.project_id=v.project_id AND s.revision=v.version_revision JOIN projects p ON p.id=v.project_id
		WHERE c.project_id=? AND c.id=?`, project, id).Scan(&owner, &source, &sourceID, &channel, &retainedHead, &complete, &attributable, &outcome, &sourceRepository, &currentRepository)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if sourceRepository != currentRepository || owner != number || source != externalHostedSource || sourceID != external || channel != "hosted" || retainedHead != head || !complete || !attributable || outcome != "completed" {
		return "", false, transferConflict("The retained hosted provider identity has contradictory source facts.")
	}
	return id, true, nil
}

func stageExternalHostedRemoval(ctx context.Context, tx *sql.Tx, write *historySnapshotWrite, number int64, head, external string) (bool, error) {
	id, exists, err := matchingExternalHostedRun(ctx, tx, write.project, number, head, external)
	if err != nil || !exists {
		return false, err
	}
	if err := write.remove(ctx, tx, historyRunRecord, []any{id}); err != nil {
		return false, err
	}
	return true, nil
}

// ReconcileNativeHostedSource repairs a previously duplicated current projection
// using an exact provider alias proven by the native capture owner. It neither
// changes native decisions nor turns imported history into native evidence.
// Without a complete current native replacement, no observer record is hidden.
func ReconcileNativeHostedSource(ctx context.Context, tx *sql.Tx, scope core.Scope, number int64, head, nativeSourceRunID, externalSourceRunID string) (Receipt, bool, error) {
	if ctx == nil || tx == nil || !validText(scope.ProjectID, 128, true) || number < 1 || number > 2147483647 || head == "" || !validSHA(head) || !validText(nativeSourceRunID, 256, true) || !externalHostedIdentity.MatchString(externalSourceRunID) {
		return Receipt{}, false, validationError("Select an exact native hosted comparison and provider identity.")
	}
	var previous, observed string
	var revision, repositoryID, currentRepositoryID int64
	err := tx.QueryRowContext(ctx, `SELECT w.snapshot_id,w.revision,s.repository_id,s.observed_at,p.github_repository_id FROM review_workspace w
		JOIN review_snapshots s ON s.project_id=w.project_id AND s.snapshot_id=w.snapshot_id JOIN projects p ON p.id=w.project_id
		WHERE w.project_id=?`, scope.ProjectID).Scan(&previous, &revision, &repositoryID, &observed, &currentRepositoryID)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, err
	}
	current := Receipt{ID: previous, Revision: revision, Outcome: "observed"}
	if repositoryID != currentRepositoryID {
		return Receipt{}, false, transferConflict("The retained review repository identity changed.")
	}
	var owner, nativeRepository int64
	var source, sourceID, channel, retainedHead, outcome string
	var complete, attributable bool
	err = tx.QueryRowContext(ctx, `SELECT v.pull_number,v.source,v.source_run_id,v.channel,v.head_sha,v.complete,v.attributable,v.outcome,s.repository_id
		FROM review_current_runs c JOIN review_run_memberships m ON m.project_id=c.project_id AND m.id=c.id AND m.start_revision=c.start_revision
		JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision
		JOIN review_snapshots s ON s.project_id=v.project_id AND s.revision=v.version_revision
		WHERE c.project_id=? AND c.id=?`, scope.ProjectID, runOpaqueID(scope.ProjectID, nativeSource, nativeSourceRunID)).Scan(&owner, &source, &sourceID, &channel, &retainedHead, &complete, &attributable, &outcome, &nativeRepository)
	if errors.Is(err, sql.ErrNoRows) {
		return current, false, nil
	}
	if err != nil {
		return Receipt{}, false, err
	}
	if nativeRepository != repositoryID || owner != number || source != nativeSource || sourceID != nativeSourceRunID || channel != "hosted" || retainedHead != head || !complete || !attributable || outcome != "completed" {
		return Receipt{}, false, transferConflict("The current native replacement does not match the hosted comparison.")
	}
	_, exists, err := matchingExternalHostedRun(ctx, tx, scope.ProjectID, number, head, externalSourceRunID)
	if err != nil || !exists {
		return current, false, err
	}
	if revision == math.MaxInt64 {
		return Receipt{}, false, core.Fail("revision_exhausted", "The review workspace revision cannot advance further.", 409)
	}
	write := newHistorySnapshotWrite(scope.ProjectID, revision+1)
	body, position, err := retainedPullBody(ctx, tx, scope.ProjectID, previous, number)
	if err != nil {
		return Receipt{}, false, err
	}
	id, next := core.NewID("review_snapshot"), revision+1
	digest := sha256.Sum256([]byte(previous + "\x00" + nativeSourceRunID + "\x00" + externalSourceRunID))
	if _, err = tx.ExecContext(ctx, `INSERT INTO review_snapshots(project_id,snapshot_id,revision,repository_id,source,observed_at,recorded_at,payload_sha256) VALUES(?,?,?,?,?,?,?,?)`, scope.ProjectID, id, next, repositoryID, nativeSource, observed, time.Now().UTC().Format(time.RFC3339Nano), hex.EncodeToString(digest[:])); err != nil {
		return Receipt{}, false, err
	}
	changed, err := stageExternalHostedRemoval(ctx, tx, write, number, head, externalSourceRunID)
	if err != nil {
		return Receipt{}, false, err
	}
	if !changed {
		return Receipt{}, false, historyInconsistent()
	}
	if err = rebuildNativeSummary(ctx, tx, write, id, number, position, body); err != nil {
		return Receipt{}, false, err
	}
	if err = write.applyCurrent(ctx, tx, id, revision); err != nil {
		return Receipt{}, false, err
	}
	if err = core.AppendEvent(ctx, tx, scope, "review.observed", id, next, map[string]any{"snapshot_id": id, "pull_number": number, "source": nativeSource, "hosted_identity_reconciled": true}); err != nil {
		return Receipt{}, false, err
	}
	if err = publicsite.Refresh(ctx, tx, scope.ProjectID, PublicSiteReviewFacts); err != nil {
		return Receipt{}, false, err
	}
	return Receipt{ID: id, Revision: next, Outcome: "observed"}, true, nil
}
