package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/benhook1013/FireController/internal/reviews"
)

const maxExternalRoundMembers = 10

// creditExternalHostedRound counts one explicitly selected, finalized batch.
// The caller owns authentication and settlement in this same writer transaction.
func creditExternalHostedRound(ctx context.Context, tx *sql.Tx, project string, pull int64, batchID string, runIDs []string, accepted int64, at string) error {
	if !roundScope(project, pull, "hosted") || !identifier.MatchString(batchID) || len(runIDs) < 1 || len(runIDs) > maxExternalRoundMembers || accepted < 0 || accepted > 100 || !roundTime(at) {
		return invalid()
	}
	ids := slices.Clone(runIDs)
	slices.Sort(ids)
	for i, id := range ids {
		if !identifier.MatchString(id) || i > 0 && ids[i-1] == id {
			return invalid()
		}
	}
	var oldProject, oldAt string
	var oldPull, oldAccepted int64
	err := tx.QueryRowContext(ctx, `SELECT project_id,pull_number,accepted,completed_at FROM review_round_external_credits WHERE batch_id=?`, batchID).Scan(&oldProject, &oldPull, &oldAccepted, &oldAt)
	if err == nil {
		if oldProject != project || oldPull != pull || oldAccepted != accepted || oldAt != at {
			return externalRoundConflict()
		}
		members, readErr := externalRoundMembers(ctx, tx, project, batchID)
		if readErr != nil {
			return readErr
		}
		if !slices.Equal(members, ids) {
			return externalRoundConflict()
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	c, err := readRoundRecord(ctx, tx, project, pull, "hosted")
	if err != nil {
		return err
	}
	if !c.Configured {
		return roundDenied("review_round_unconfigured", "Configure additional Hosted rounds before counting an external batch.")
	}
	if err = checkRoundOpen(c.RoundControl); err != nil {
		return err
	}
	if _, err = readRoundGrant(ctx, tx, project, pull, "hosted", c.grantRevision, c.Epoch); err != nil {
		return err
	}
	var busy bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_operations WHERE project_id=? AND pull_number=? AND channel='hosted' AND state IN ('preparing','posting','active','uncertain'))
 OR EXISTS(SELECT 1 FROM projects p JOIN review_execution_reservations r ON r.repository_id=p.github_repository_id WHERE p.id=? AND r.pull_number=? AND r.action_class='hosted')`, project, pull, project, pull).Scan(&busy); err != nil {
		return err
	}
	if busy {
		return roundDenied("review_round_inflight", "Finish the Hosted work already underway before counting an external batch.")
	}
	queued, adjudication, err := roundOccupancyStages(ctx, tx, c, "")
	if err != nil {
		return err
	}
	if !c.cumulative {
		adjudication, err = roundUnfinalizedOccupancy(ctx, tx, c, "")
		if err != nil {
			return err
		}
	}
	if adjudication != 0 {
		return roundDenied("review_round_inflight", "Finalize the earlier native Hosted result before counting an external batch.")
	}
	used, err := roundCompletionCount(ctx, tx, project, pull, "hosted", c.Epoch)
	if err != nil {
		return err
	}
	if used != c.Used {
		return roundUnavailable()
	}
	if !roundHasCapacity(c.RoundControl, queued) {
		return roundDenied("review_round_capacity", "The remaining Hosted rounds are reserved by queued work.")
	}
	for _, id := range ids {
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_round_external_members WHERE project_id=? AND run_id=?)`, project, id).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return externalRoundConflict()
		}
	}
	if err = validateExternalRoundAccepted(ctx, tx, project, pull, ids, accepted); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO review_round_external_credits(batch_id,project_id,pull_number,channel,epoch,revision,ordinal,accepted,completed_at) VALUES(?,?,?,'hosted',?,?,?,?,?)`, batchID, project, pull, c.Epoch, c.Revision, used+1, accepted, at); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_round_external_members(project_id,run_id,batch_id) VALUES(?,?,?)`, project, id, batchID); err != nil {
			return err
		}
	}
	return writeRoundCompletionProgress(ctx, tx, &c, accepted, at)
}

func externalRoundConflict() error {
	return roundDenied("review_round_external_conflict", "This external Hosted batch or one of its runs has already been counted.")
}

func roundCompletionCount(ctx context.Context, q EvidenceQuery, project string, pull int64, channel string, epoch int64) (int64, error) {
	var count int64
	const native = `SELECT count(*) FROM review_round_completions WHERE project_id=? AND pull_number=? AND channel=? AND epoch=?`
	if channel != "hosted" {
		err := q.QueryRowContext(ctx, native, project, pull, channel, epoch).Scan(&count)
		return count, err
	}
	err := q.QueryRowContext(ctx, `SELECT (`+native+`) + (SELECT count(*) FROM review_round_external_credits WHERE project_id=? AND pull_number=? AND channel='hosted' AND epoch=?)`, project, pull, channel, epoch, project, pull, epoch).Scan(&count)
	return count, err
}

func externalRoundMembers(ctx context.Context, q EvidenceQuery, project, batchID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT project_id,run_id FROM review_round_external_members WHERE batch_id=? ORDER BY run_id LIMIT ?`, batchID, maxExternalRoundMembers+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var owner, id string
		if err = rows.Scan(&owner, &id); err != nil {
			return nil, err
		}
		if owner != project || !identifier.MatchString(id) {
			return nil, roundUnavailable()
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) < 1 || len(ids) > maxExternalRoundMembers {
		return nil, roundUnavailable()
	}
	return ids, nil
}

func validateExternalRoundAccepted(ctx context.Context, q EvidenceQuery, project string, pull int64, ids []string, accepted int64) error {
	var sum int64
	for _, id := range ids {
		count, err := reviews.ExternalHostedFinalizedRunAccepted(ctx, q, project, pull, id)
		if err != nil {
			return err
		}
		if count < 0 || count > 100-sum {
			return roundUnavailable()
		}
		sum += count
	}
	if sum != accepted {
		return roundUnavailable()
	}
	return nil
}

// A native alias found after external settlement can still finalize its own
// evidence, but the underlying provider result cannot earn another round.
func hasCreditedNativeHostedAlias(ctx context.Context, q EvidenceQuery, project string, pull int64, hosted *HostedEvidenceMetadata) (bool, error) {
	source := nativeHostedExternalSourceID(hosted)
	if source == "" {
		return false, nil
	}
	var owner string
	var number int64
	err := q.QueryRowContext(ctx, `SELECT c.project_id,c.pull_number FROM review_round_external_members m JOIN review_round_external_credits c USING(batch_id) WHERE m.project_id=? AND m.run_id=?`, project, reviews.ExternalHostedRunID(project, source)).Scan(&owner, &number)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if owner != project || number != pull {
		return false, roundUnavailable()
	}
	return true, nil
}
