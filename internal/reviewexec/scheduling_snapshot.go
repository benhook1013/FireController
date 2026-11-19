package reviewexec

import (
	"context"
	"database/sql"
	"github.com/benhook1013/FireController/internal/strictjson"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
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
	// A retained projection cannot predate a matching scheduling event.
	var ahead bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_scheduling s WHERE EXISTS(
		SELECT 1 FROM events e WHERE e.project_id=s.project_id AND e.resource_id=s.project_id
		AND e.event_type='review.scheduling_updated' AND e.revision>s.revision))`).Scan(&ahead)
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
	for rows.Next() {
		var s PullScheduling
		if err = scanSchedulingSnapshot(rows, &s.ProjectID, &s.PullNumber, &s.Revision, &s.Paused, &s.Reason, &s.ActorID, &s.UpdatedAt); err != nil {
			break
		}
		if !identifier.MatchString(s.ProjectID) || s.PullNumber < 1 || s.PullNumber > 2147483647 || s.Revision < 1 || !identifier.MatchString(s.ActorID) || !roundTime(s.UpdatedAt) || !validSchedulingReason(s.Reason) || !s.Paused && s.Reason != "" {
			err = schedulingUnavailable()
			break
		}
		if err = validatePullSchedulingEvent(ctx, tx, s); err != nil {
			break
		}
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
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_automatic_selections a LEFT JOIN review_selections s ON s.id=a.selection_id LEFT JOIN review_scheduling c ON c.project_id=a.project_id LEFT JOIN review_operations o ON o.id=s.operation_id WHERE s.id IS NULL OR s.project_id<>a.project_id OR s.channel NOT IN(`+channels+`) OR s.actor_id<>a.actor_id OR s.worker_id<>a.worker_id OR c.project_id IS NULL OR a.scheduling_revision>c.revision OR (a.pull_number=0 AND (a.pull_basis_sha256<>'' OR s.state='selected')) OR (a.pull_number<>0 AND (s.state<>'selected' OR o.id IS NULL OR o.pull_number<>a.pull_number OR o.project_id<>a.project_id OR o.channel<>s.channel OR o.actor_id<>a.actor_id OR o.worker_id<>a.worker_id)))`).Scan(&bad)
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
func validateOrphanPullSchedulingEvents(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `WITH orphan AS (
		SELECT e.project_id,e.resource_id,e.revision,
			CASE WHEN typeof(e.payload_json)='text' AND length(CAST(e.payload_json AS BLOB))<=4096 THEN e.payload_json END AS payload,
			ROW_NUMBER() OVER (PARTITION BY e.project_id,e.resource_id ORDER BY e.revision DESC,e.sequence DESC) AS newest,
			COUNT(*) OVER (PARTITION BY e.project_id,e.resource_id,e.revision) AS matches
		FROM events e LEFT JOIN review_pull_scheduling s
			ON s.project_id=e.project_id AND s.pull_number=CAST(e.resource_id AS INTEGER)
			AND CAST(s.pull_number AS TEXT)=e.resource_id
		WHERE e.event_type='review.pull_scheduling_updated' AND s.pull_number IS NULL
	)
	SELECT resource_id,revision,payload,matches FROM orphan WHERE newest=1`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var resource string
		var revision, matches int64
		var payload sql.NullString
		if err := rows.Scan(&resource, &revision, &payload, &matches); err != nil {
			return err
		}
		pull, err := strconv.ParseInt(resource, 10, 32)
		if err != nil || pull < 1 || strconv.FormatInt(pull, 10) != resource || revision < 1 || matches != 1 ||
			!validPullSchedulingPayload(payload, pull, false) {
			return schedulingUnavailable()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return rows.Close()
}

func validPullSchedulingPayload(payload sql.NullString, pull int64, paused bool) bool {
	var event struct {
		PullNumber int64 `json:"pull_number"`
		Paused     *bool `json:"paused"`
	}
	return payload.Valid && strictjson.Decode([]byte(payload.String), &event) == nil &&
		event.PullNumber == pull && event.Paused != nil && *event.Paused == paused
}

// A restored control must agree with its latest retained user decision.
func validatePullSchedulingEvent(ctx context.Context, tx *sql.Tx, s PullScheduling) error {
	rows, err := tx.QueryContext(ctx, `SELECT revision,actor_id,CASE WHEN typeof(payload_json)='text' AND length(CAST(payload_json AS BLOB))<=4096 THEN payload_json END FROM events WHERE project_id=? AND event_type='review.pull_scheduling_updated' AND resource_id=? AND revision>=? ORDER BY revision DESC,sequence DESC LIMIT 2`, s.ProjectID, strconv.FormatInt(s.PullNumber, 10), s.Revision)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		var revision int64
		var actor string
		var payload sql.NullString
		if err := rows.Scan(&revision, &actor, &payload); err != nil {
			return err
		}
		if count != 1 || revision != s.Revision || actor != s.ActorID || !validPullSchedulingPayload(payload, s.PullNumber, s.Paused) {
			return schedulingUnavailable()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count != 1 {
		return schedulingUnavailable()
	}
	return rows.Close()
}
