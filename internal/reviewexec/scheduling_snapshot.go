package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// ValidateSchedulingSnapshot checks retained control/attribution, not current
// authority: revoked credentials and retired authors remain valid history.
// Accept the original Hosted-only domain and the additive CLI setting.
func ValidateSchedulingSnapshot(ctx context.Context, tx *sql.Tx) error {
	var version string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:review_scheduling'`).Scan(&version); err != nil {
		return err
	}
	cli := "0"
	channels := "'hosted'"
	switch version {
	case "1":
		// Version one predates automatic CLI scheduling.
	case "2":
		cli = "automatic_cli"
		channels = "'hosted','cli'"
	default:
		return schedulingUnavailable()
	}
	rows, err := tx.QueryContext(ctx, `SELECT project_id,revision,automatic_hosted,`+cli+`,actor_id,worker_id,credential_id,updated_at FROM review_scheduling`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var project, actor, worker, credential, at string
		var revision int64
		var enabled, cliEnabled bool
		if err = scanSchedulingSnapshot(rows, &project, &revision, &enabled, &cliEnabled, &actor, &worker, &credential, &at); err != nil {
			break
		}
		hostedEnabled := enabled
		enabled = enabled || cliEnabled
		if !identifier.MatchString(project) || revision < 1 || !identifier.MatchString(actor) || !roundTime(at) || worker != "" && !identifier.MatchString(worker) || enabled && (!identifier.MatchString(worker) || !identifier.MatchString(credential)) || !enabled && credential != "" {
			err = schedulingUnavailable()
			break
		}
		_, _, err = schedulingBasisRevisions(ctx, tx, schedulingRecord{Scheduling: Scheduling{ProjectID: project, Revision: revision, AutomaticHosted: hostedEnabled, AutomaticCLI: cliEnabled}, actor: actor})
		if err != nil {
			break
		}
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// A genuine project scheduling event must have a projection at least
	// as new. Only projects with no retained history default to revision zero.
	var ahead bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events e
		LEFT JOIN review_scheduling s ON s.project_id=e.project_id
		WHERE e.event_type='review.scheduling_updated'
		AND (typeof(e.resource_id)<>'text' OR e.resource_id<>e.project_id OR s.project_id IS NULL OR e.revision>s.revision))`).Scan(&ahead)
	if err != nil {
		return err
	}
	if ahead {
		return schedulingUnavailable()
	}

	rows, err = tx.QueryContext(ctx, `SELECT project_id,pull_number,revision,paused,reason,actor_id,updated_at FROM review_pull_scheduling`)
	if err != nil {
		return err
	}
	controls := 0
	for rows.Next() {
		var s PullScheduling
		if err = scanSchedulingSnapshot(rows, &s.ProjectID, &s.PullNumber, &s.Revision, &s.Paused, &s.Reason, &s.ActorID, &s.UpdatedAt); err != nil {
			break
		}
		if !identifier.MatchString(s.ProjectID) || s.PullNumber < 1 || s.PullNumber > 2147483647 || s.Revision < 1 || !identifier.MatchString(s.ActorID) || !roundTime(s.UpdatedAt) || !validSchedulingReason(s.Reason) || !s.Paused && s.Reason != "" {
			err = schedulingUnavailable()
			break
		}
		controls++
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr = rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := validatePullSchedulingEvents(ctx, tx, controls); err != nil {
		return err
	}
	if err := validateOrphanPullSchedulingEvents(ctx, tx); err != nil {
		return err
	}
	var bad bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_scheduling s WHERE (s.worker_id<>'' AND NOT EXISTS(SELECT 1 FROM workers w WHERE w.project_id=s.project_id AND w.id=s.worker_id)) OR ((s.automatic_hosted=1 OR `+strings.ReplaceAll(cli, "automatic_cli", "s.automatic_cli")+`=1) AND NOT EXISTS(SELECT 1 FROM credentials c WHERE c.id=s.credential_id AND c.principal_id=s.actor_id))) OR EXISTS(SELECT 1 FROM review_automatic_selections a WHERE NOT EXISTS(SELECT 1 FROM workers w WHERE w.project_id=a.project_id AND w.id=a.worker_id) OR NOT EXISTS(SELECT 1 FROM credentials c WHERE c.id=a.credential_id AND c.principal_id=a.actor_id))`).Scan(&bad)
	if err != nil {
		return err
	}
	if bad {
		return schedulingUnavailable()
	}
	var selectionVersion string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:review_selection'`).Scan(&selectionVersion); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	custodyJoin, linked, held := "", "s.operation_id", "0"
	if selectionVersion == "2" {
		custodyJoin = " LEFT JOIN review_selection_quarantine q ON q.selection_id=s.id AND q.project_id=s.project_id"
		linked = "COALESCE(s.operation_id,q.original_operation_id)"
		// The record is only a backup provenance exception. Runtime readers
		// separately fence this operation; the held row cannot authorize it.
		held = "(s.state='held' AND s.reason='linked_selection_quarantined' AND s.operation_id IS NULL AND q.selection_id IS NOT NULL)"
	}
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_automatic_selections a LEFT JOIN review_selections s ON s.id=a.selection_id`+custodyJoin+`
		LEFT JOIN review_scheduling c ON c.project_id=a.project_id LEFT JOIN review_operations o ON o.id=`+linked+`
		WHERE s.id IS NULL OR s.project_id<>a.project_id OR s.channel NOT IN(`+channels+`) OR s.actor_id<>a.actor_id OR s.worker_id<>a.worker_id OR c.project_id IS NULL
		OR a.scheduling_revision>c.revision OR (a.pull_number=0 AND (a.pull_basis_sha256<>'' OR s.state='selected'))
		OR (a.pull_number<>0 AND ((s.state<>'selected' AND NOT `+held+`) OR o.id IS NULL OR o.pull_number<>a.pull_number
			OR o.project_id<>a.project_id OR o.channel<>s.channel OR o.actor_id<>a.actor_id OR o.worker_id<>a.worker_id)))`).Scan(&bad)
	if err != nil {
		return err
	}
	if bad {
		return schedulingUnavailable()
	}
	rows, err = tx.QueryContext(ctx, `SELECT selection_id,project_id,scheduling_revision,basis_sha256,settled_basis_sha256,pull_number,pull_basis_sha256,actor_id,worker_id,credential_id FROM review_automatic_selections`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var selection, project, basis, settled, pullBasis, actor, worker, credential string
		var revision, pull int64
		if err = scanSchedulingSnapshot(rows, &selection, &project, &revision, &basis, &settled, &pull, &pullBasis, &actor, &worker, &credential); err != nil {
			return err
		}
		if !validRunID(selection) || !identifier.MatchString(project) || revision < 1 || !hash.MatchString(basis) || settled != "" && !hash.MatchString(settled) || pull < 0 || pull > 2147483647 || pull > 0 && !hash.MatchString(pullBasis) || !identifier.MatchString(actor) || !identifier.MatchString(worker) || !identifier.MatchString(credential) {
			return core.Fail("storage_unavailable", "Stored automatic review attribution is unavailable.", 503)
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return rows.Close()
}

// Preserve SQLite storage types before checking control and attribution values.
// database/sql otherwise accepts conversions (notably BLOB to string) that the
// live control readers deliberately reject. Genuine scan/query errors propagate.
func scanSchedulingSnapshot(row interface{ Scan(...any) error }, destinations ...any) error {
	values := make([]any, len(destinations))
	pointers := make([]any, len(values))
	for i := range values {
		pointers[i] = &values[i]
	}
	if err := row.Scan(pointers...); err != nil {
		return err
	}
	for i, destination := range destinations {
		switch target := destination.(type) {
		case *string:
			value, ok := values[i].(string)
			if !ok {
				return schedulingUnavailable()
			}
			*target = value
		case *int64:
			value, ok := values[i].(int64)
			if !ok {
				return schedulingUnavailable()
			}
			*target = value
		case *bool:
			value, ok := values[i].(int64)
			if !ok || value < 0 || value > 1 {
				return schedulingUnavailable()
			}
			*target = value == 1
		default:
			return schedulingUnavailable()
		}
	}
	return nil
}

// An absent control reads as resumed. Refuse a restore if its latest
// retained event instead proves a pause or cannot be read as that identity.
// Rank every event before projecting its native revision; never fall back to
// an older valid resume when the winning revision cannot be trusted.
const orphanPullSchedulingSQL = `WITH orphan_history AS (
        SELECT e.project_id,e.resource_id,e.revision,e.sequence,e.event_type,
            CASE WHEN typeof(e.payload_json)='text' AND length(CAST(e.payload_json AS BLOB))<=4096 THEN e.payload_json END AS payload,
            LAG(e.revision) OVER (PARTITION BY e.project_id,e.resource_id ORDER BY e.sequence) AS previous_revision
        FROM events e LEFT JOIN review_pull_scheduling s
            ON s.project_id=e.project_id AND s.pull_number=CAST(e.resource_id AS INTEGER)
            AND CAST(s.pull_number AS TEXT)=e.resource_id
        WHERE (e.event_type='review.pull_scheduling_updated'
            OR (typeof(e.event_type)='blob' AND CAST(e.event_type AS TEXT)='review.pull_scheduling_updated'))
            AND s.pull_number IS NULL
    ), orphan AS (
        SELECT project_id,resource_id,revision,sequence,payload,
            ROW_NUMBER() OVER (PARTITION BY project_id,resource_id ORDER BY revision DESC,sequence DESC) AS newest,
            COUNT(*) OVER (PARTITION BY project_id,resource_id,revision) AS matches,
            MAX(CASE WHEN typeof(event_type)<>'text' OR typeof(revision)<>'integer' OR revision<1
                OR (previous_revision IS NOT NULL AND revision<=previous_revision) THEN 1 ELSE 0 END)
                OVER (PARTITION BY project_id,resource_id) AS bad_revision
        FROM orphan_history
    )`

const latestOrphanPullResumesSQL = `
	FROM orphan o JOIN events e ON e.sequence=o.sequence
	WHERE o.newest=1`

func validateOrphanPullSchedulingEvents(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, orphanPullSchedulingSQL+`
	SELECT CASE WHEN typeof(o.project_id)='text' THEN o.project_id END,o.resource_id,
		CASE WHEN o.bad_revision=0 THEN o.revision END,o.payload,o.matches,
		CASE WHEN typeof(e.actor_id)='text' AND length(CAST(e.actor_id AS BLOB))<=128 THEN e.actor_id END,
		CASE WHEN typeof(e.created_at)='text' AND length(CAST(e.created_at AS BLOB))<=64 THEN e.created_at END`+latestOrphanPullResumesSQL)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := validateOrphanPullSchedulingEvent(rows); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return rows.Close()
}

func validateOrphanPullSchedulingEvent(row interface{ Scan(...any) error }) error {
	var storedResource, revision any
	var matches int64
	var project, payload, actor, createdAt sql.NullString
	if err := row.Scan(&project, &storedResource, &revision, &payload, &matches, &actor, &createdAt); err != nil {
		return err
	}
	resource, ok := storedResource.(string)
	if !ok || len(resource) > 10 {
		return schedulingUnavailable()
	}
	nativeRevision, revisionOK := revision.(int64)
	pull, err := strconv.ParseInt(resource, 10, 32)
	if err != nil || pull < 1 || strconv.FormatInt(pull, 10) != resource || !revisionOK || nativeRevision < 1 || matches != 1 ||
		!validPullSchedulingPayload(payload, pull, false) || !project.Valid || !identifier.MatchString(project.String) ||
		!actor.Valid || !identifier.MatchString(actor.String) || !createdAt.Valid || !roundTime(createdAt.String) {
		return schedulingUnavailable()
	}
	return nil
}

// RestoreHistoricalPullResumes preserves the CAS revision of valid legacy
// resume events on an offline restored copy. The caller owns its restore
// transaction. No event, credential, or project scheduling authority changes.
func RestoreHistoricalPullResumes(ctx context.Context, tx *sql.Tx) error {
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='restore:state'`).Scan(&state); err != nil {
		return err
	}
	if state != "inactive" {
		return schedulingUnavailable()
	}
	var version string
	err := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:review_scheduling'`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		// Older backups may omit the scheduling domain altogether.
		return nil
	}
	if err != nil {
		return err
	}
	if version != "1" && version != "2" {
		return schedulingUnavailable()
	}
	if err := validateOrphanPullSchedulingEvents(ctx, tx); err != nil {
		return err
	}
	// Both reads are in the same transaction, and no projection was inserted
	// until the complete set was validated. Keep the source event sequence.
	_, err = tx.ExecContext(ctx, orphanPullSchedulingSQL+`
	INSERT INTO review_pull_scheduling(project_id,pull_number,revision,paused,reason,actor_id,updated_at)
	SELECT o.project_id,CAST(o.resource_id AS INTEGER),o.revision,0,'',e.actor_id,e.created_at`+latestOrphanPullResumesSQL)
	return err
}

func validPullSchedulingPayload(payload sql.NullString, pull int64, paused bool) bool {
	var event struct {
		PullNumber int64 `json:"pull_number"`
		Paused     *bool `json:"paused"`
	}
	return payload.Valid && strictjson.Decode([]byte(payload.String), &event) == nil &&
		event.PullNumber == pull && event.Paused != nil && *event.Paused == paused
}

// Scan the event journal once for existing pull controls. With events on the
// outer side of this join, SQLite can seek the control's composite primary key
// rather than reading the same project's event history once for each control.
// Ordered qualifying matches keep duplicate detection constant-space.
const pullSchedulingSnapshotEventsSQL = `SELECT s.project_id,s.pull_number,s.revision,s.paused,s.actor_id,
	e.revision,e.actor_id,CASE WHEN typeof(e.payload_json)='text' AND length(CAST(e.payload_json AS BLOB))<=4096 THEN e.payload_json END
	FROM events e CROSS JOIN review_pull_scheduling s
	WHERE e.event_type='review.pull_scheduling_updated' AND s.project_id=e.project_id
	AND s.pull_number=CAST(e.resource_id AS INTEGER) AND CAST(s.pull_number AS TEXT)=e.resource_id
	AND (typeof(e.revision)<>'integer' OR e.revision<1 OR e.revision>=s.revision)
	ORDER BY s.project_id,s.pull_number`

func validatePullSchedulingEvents(ctx context.Context, tx *sql.Tx, controls int) error {
	if controls == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, pullSchedulingSnapshotEventsSQL)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	var lastProject string
	var lastPull int64
	for rows.Next() {
		var project, actor string
		var pull, controlRevision, paused int64
		var nativeRevision, nativeEventActor any
		var payload sql.NullString
		if err := rows.Scan(&project, &pull, &controlRevision, &paused, &actor, &nativeRevision, &nativeEventActor, &payload); err != nil {
			return err
		}
		revision, integer := nativeRevision.(int64)
		eventActor, textActor := nativeEventActor.(string)
		if (project == lastProject && pull == lastPull) || !integer || !textActor || revision != controlRevision || eventActor != actor || !validPullSchedulingPayload(payload, pull, paused == 1) {
			return schedulingUnavailable()
		}
		count++
		lastProject, lastPull = project, pull
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count != controls {
		return schedulingUnavailable()
	}
	return rows.Close()
}
