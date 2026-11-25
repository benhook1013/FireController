package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const automaticSourceRetryDelay = 30 * time.Second

// The selection's retained monotonic revision is a conservative transition
// budget, not a retry counter. An ordinary failure is revision 3; each complete
// requeue/claim/failure costs three more revisions. Allowing only revisions <= 6
// permits at most two delayed retries of completed source holds. Restart
// recovery consumes this budget too, never replenishes it. Existing backup/restore retains both the
// revision and updated_at, so neither process restarts nor restore reset it.
func transientAutomaticSourceHold(r Run) bool {
	if !validChannel(r.Channel) || r.State != "held" || r.OperationID != "" || r.Revision < 3 || r.Revision > 6 {
		return false
	}
	switch r.Reason {
	case "source_budget_exhausted", "source_fetch_timeout", "source_provider_unavailable", "routed_findings_unavailable":
		return true
	}
	return false
}

// retryAutomaticSourceTx only requeues a previously accepted read-only intent.
// It is called after the normal coordinator's restore, authorizer, pause and
// active-operation checks, and only for an already suppressed semantic basis.
// No operation, attempt, grant or new selection is created here.
func (e *Engine) retryAutomaticSourceTx(ctx context.Context, tx *sql.Tx, project, basis string, schedulingRevision int64, now time.Time) (bool, error) {
	return e.retryAutomaticSourceChannelTx(ctx, tx, project, basis, schedulingRevision, now, "hosted")
}

func (e *Engine) retryAutomaticSourceChannelTx(ctx context.Context, tx *sql.Tx, project, basis string, schedulingRevision int64, now time.Time, channel string) (bool, error) {
	r, err := e.automaticSourceRetryCandidate(ctx, tx, project, basis, schedulingRevision, now, channel)
	if err != nil || r == nil {
		return false, err
	}
	if err = updateRunAt(ctx, tx, *r, "queued", "", "", now); err != nil {
		return false, err
	}
	return true, nil
}

// This read-only check is shared by preflight and writer revalidation.
func (e *Engine) automaticSourceRetryCandidate(ctx context.Context, tx *sql.Tx, project, basis string, schedulingRevision int64, now time.Time, channel string) (*Run, error) {
	if !validChannel(channel) {
		return nil, schedulingUnavailable()
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT s.id FROM review_selections s JOIN review_automatic_selections a ON a.selection_id=s.id AND a.project_id=s.project_id WHERE s.project_id=? AND s.channel=? AND s.state='held' AND s.reason<>'linked_selection_quarantined' AND s.operation_id IS NULL AND s.revision BETWEEN 3 AND 6 AND a.scheduling_revision=? AND (a.basis_sha256=? OR a.settled_basis_sha256=?) ORDER BY s.created_at DESC,s.id LIMIT 1`, project, channel, schedulingRevision, basis, basis).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r, err := readRun(ctx, tx, project, id)
	if err != nil {
		return nil, err
	}
	if !transientAutomaticSourceHold(r) {
		return nil, nil
	}
	updated, err := time.Parse(time.RFC3339Nano, r.UpdatedAt)
	if err != nil {
		return nil, runUnavailable()
	}
	if now.Before(updated.Add(automaticSourceRetryDelay)) {
		return nil, nil
	}
	superseded, err := automaticSourceSuperseded(ctx, tx, r)
	if err != nil || superseded {
		return nil, err
	}
	configuration, _, reason, err := e.runBinding(ctx, tx, r)
	if err != nil || reason != "" {
		return nil, err
	}
	if r.Reason == "routed_findings_unavailable" {
		// A repaired projection can have the same source digest as before
		// invalidation. Reconsider the same intent, but do not spend its
		// bounded source retries while a configured pull's route facts are
		// dirty. Check the whole bounded queue: an eligible pull may depend
		// on an ancestor outside its current waiting cutoff.
		var dirty bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM json_each(?) q JOIN review_route_policy_invalidations i
			ON i.project_id=? AND i.target_pull_number=q.value)`, encoded(configuration.Queue), project).Scan(&dirty)
		if err != nil || dirty {
			return nil, err
		}
	}
	return &r, nil
}

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
	if err != nil {
		return "", err
	}
	if initial {
		proved, err := verifiedInitialAutomaticSourceAcceptance(ctx, tx, r)
		if err != nil {
			return "", err
		}
		if !proved {
			return "selection_superseded", nil
		}
		return "", nil
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

// An interrupted read can relax exact-revision continuity only when this
// selection durably recorded its automatic acceptance. The common decoder
// validates the unique revision-one event and binds its actor, worker and lane.
func verifiedInitialAutomaticSourceAcceptance(ctx context.Context, q EvidenceQuery, r Run) (bool, error) {
	event, err := readSelectionRequestedEvent(ctx, q, r)
	if err != nil {
		return false, err
	}
	return event.sequence > 0 && event.Origin == selectionOriginAutomatic, nil
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
	// Event sequence is the durable acceptance order even if the wall clock
	// moves backwards. Legacy eventless rows are readable, but their timestamps
	// cannot prove they preceded this intent. Treat their order as unknown.
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
 AND NOT EXISTS(SELECT 1 FROM events e WHERE e.project_id=s.project_id AND e.resource_id=s.id AND e.event_type='review.selection_requested' AND e.revision=1)
) OR EXISTS(
 SELECT 1 FROM review_operations o WHERE o.project_id=? AND o.channel=?
 AND NOT EXISTS(SELECT 1 FROM events e WHERE e.project_id=o.project_id AND e.resource_id=o.id AND e.event_type='review.requested' AND e.revision=1)
) OR EXISTS(
 SELECT 1 FROM events e JOIN review_selections s ON s.project_id=e.project_id AND s.id=e.resource_id
 WHERE e.project_id=? AND e.sequence>? AND e.event_type='review.selection_requested' AND e.revision=1 AND s.channel=?
) OR EXISTS(
 SELECT 1 FROM events e JOIN review_operations o ON o.project_id=e.project_id AND o.id=e.resource_id
 WHERE e.project_id=? AND e.sequence>? AND e.event_type='review.requested' AND e.revision=1 AND o.channel=?
)`, r.ProjectID, r.Channel, r.ID, r.ProjectID, r.Channel, r.ProjectID, accepted, r.Channel, r.ProjectID, accepted, r.Channel).Scan(&superseded)
	return superseded, err
}
