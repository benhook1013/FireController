package reviewexec

import (
	"context"
	"database/sql"
	"errors"
)

// Recheck recovery fences during every admission snapshot, including after
// source I/O. Interrupted initial reads retain ordinary authorization continuity.
// Completed source holds require the exact accepted scheduling revision even
// after further interrupted reads; incomplete transition evidence is conservative.
func automaticSourceRecoveryFence(ctx context.Context, tx *sql.Tx, r Run) (string, error) {
	if !validChannel(r.Channel) || r.Revision < 4 {
		return "", nil
	}
	var retainedRevision any
	err := tx.QueryRowContext(ctx, `SELECT scheduling_revision FROM review_automatic_selections WHERE project_id=? AND selection_id=?`, r.ProjectID, r.ID).Scan(&retainedRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// Compare native SQLite integers, not values coerced by Scan into int64.
	accepted, validRevision := retainedRevision.(int64)
	if !validRevision || accepted < 1 {
		return "", schedulingUnavailable()
	}
	current, err := readScheduling(ctx, tx, r.ProjectID)
	if err != nil {
		return "", err
	}
	if current.Revision == 0 {
		return "automation_authorization_changed", nil
	}
	initial, err := automaticSourceInitialRead(ctx, tx, r)
	if err != nil || initial {
		return "", err
	}
	if accepted != current.Revision {
		return "automation_authorization_changed", nil
	}
	superseded, err := automaticSourceSuperseded(ctx, tx, r)
	if err != nil {
		return "", err
	}
	if superseded {
		return "selection_superseded", nil
	}
	return "", nil
}

// A revision alone cannot distinguish restart recovery from a completed hold.
// Prove every retained transition was an unfinished read before relaxing the
// exact-revision fence. A held event (whatever its reason), missing/duplicate
// revision or malformed payload keeps that fence. Aggregate in SQL rather than
// copying an unbounded event history into each admission snapshot.
func automaticSourceInitialRead(ctx context.Context, tx *sql.Tx, r Run) (bool, error) {
	var count, distinct, first, last, invalid int64
	err := tx.QueryRowContext(ctx, `SELECT count(*),count(DISTINCT revision),COALESCE(min(revision),0),COALESCE(max(revision),0),COALESCE(sum(
 CASE WHEN typeof(revision)='integer' AND json_valid(payload_json) THEN
  CASE WHEN json_extract(payload_json,'$.state')=CASE WHEN revision%2=0 THEN 'selecting' ELSE 'queued' END
  AND json_extract(payload_json,'$.reason')='' AND (revision<>? OR json_extract(payload_json,'$.state')=?) THEN 0 ELSE 1 END
 ELSE 1 END),0)
 FROM events WHERE project_id=? AND resource_id=? AND event_type='review.selection_updated' AND revision BETWEEN 2 AND ?`, r.Revision, r.State, r.ProjectID, r.ID, r.Revision).Scan(&count, &distinct, &first, &last, &invalid)
	if err != nil {
		return false, err
	}
	return count == r.Revision-1 && distinct == count && first == 2 && last == r.Revision && invalid == 0, nil
}

func automaticSourceSuperseded(ctx context.Context, tx *sql.Tx, r Run) (bool, error) {
	// Any later or simultaneous same-channel intent supersedes this recovery, even if
	// that intent was cancelled, denied, held or completed. A directly submitted
	// operation is also a fence; never recover behind its cancellation/uncertainty.
	// Compare parsed times, not RFC3339 text with variable fractional precision.
	// For rows with no acceptance event, millisecond ties conservatively fence.
	// Event sequence is the durable acceptance order even if the wall clock
	// moves backwards. Missing acceptance evidence is not permission to retry.
	var accepted int64
	err := tx.QueryRowContext(ctx, `SELECT sequence FROM events WHERE project_id=? AND resource_id=? AND event_type='review.selection_requested' AND revision=1 ORDER BY sequence DESC LIMIT 1`, r.ProjectID, r.ID).Scan(&accepted)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var superseded bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM review_selections s WHERE s.project_id=? AND s.channel=? AND s.id<>?
 AND (julianday(s.created_at) IS NULL OR julianday(s.created_at)>=julianday(?))
 AND NOT EXISTS(SELECT 1 FROM events e WHERE e.project_id=s.project_id AND e.resource_id=s.id AND e.event_type='review.selection_requested' AND e.revision=1)
) OR EXISTS(
 SELECT 1 FROM review_operations o WHERE o.project_id=? AND o.channel=? AND (julianday(o.created_at) IS NULL OR julianday(o.created_at)>=julianday(?))
 AND NOT EXISTS(SELECT 1 FROM events e WHERE e.project_id=o.project_id AND e.resource_id=o.id AND e.event_type='review.requested' AND e.revision=1)
) OR EXISTS(
 SELECT 1 FROM events e JOIN review_selections s ON s.project_id=e.project_id AND s.id=e.resource_id
 WHERE e.project_id=? AND e.sequence>? AND e.event_type='review.selection_requested' AND e.revision=1 AND s.channel=?
) OR EXISTS(
 SELECT 1 FROM events e JOIN review_operations o ON o.project_id=e.project_id AND o.id=e.resource_id
 WHERE e.project_id=? AND e.sequence>? AND e.event_type='review.requested' AND e.revision=1 AND o.channel=?
)`, r.ProjectID, r.Channel, r.ID, r.CreatedAt, r.ProjectID, r.Channel, r.CreatedAt, r.ProjectID, accepted, r.Channel, r.ProjectID, accepted, r.Channel).Scan(&superseded)
	return superseded, err
}
