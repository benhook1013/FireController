package reviewexec

import (
	"context"
	"database/sql"
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
