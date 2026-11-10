package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
	"slices"
	"time"
)

func (e *Engine) generateAutomaticCLI(ctx context.Context, readers map[string]TargetReader) (bool, error) {
	var projects []string
	if err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT project_id FROM review_scheduling WHERE automatic_cli=1 ORDER BY (SELECT lower(repository) FROM projects WHERE id=project_id)<>'benhook1013/firemud',(project_id<=?),project_id LIMIT 100`, e.automaticCLICursor)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			if err = rows.Scan(&p); err != nil {
				return err
			}
			projects = append(projects, p)
		}
		return rows.Err()
	}); err != nil {
		return false, err
	}
	for _, project := range projects {
		e.automaticCLICursor = project
		worked := false
		err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			var restored bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metadata WHERE (key='restore:provider_state' AND value='held') OR (key='restore:state' AND value='inactive'))`).Scan(&restored); err != nil {
				return err
			}
			if restored {
				return nil
			}
			s, err := readScheduling(ctx, tx, project)
			if err != nil {
				return err
			}
			if !s.AutomaticCLI {
				return nil
			}
			scope, err := e.store.ProjectAutomationScope(ctx, tx, project, s.actor, s.AuthorizingWorkerID, s.credential)
			if err != nil {
				var failure *core.Error
				if errors.As(err, &failure) && failure.Status >= 400 && failure.Status < 500 {
					return nil
				}
				return err
			}
			c, _, err := readConfiguration(ctx, tx, project)
			if err != nil {
				var failure *core.Error
				if errors.As(err, &failure) && failure.Status == 409 {
					return nil
				}
				return err
			}
			if c.Paused || !slices.Contains(c.Channels, "cli") || e.validateConfiguration(c) != nil || readers[c.CapabilityID] == nil || len(c.Queue) == 0 {
				return nil
			}
			unpaused := false
			for _, pull := range c.Queue {
				p, err := readPullScheduling(ctx, tx, project, pull)
				if err != nil {
					return err
				}
				unpaused = unpaused || !p.Paused
			}
			if !unpaused {
				return nil
			}
			policy, err := readPolicy(ctx, tx, project)
			if err != nil {
				var failure *core.Error
				if errors.As(err, &failure) && failure.Code == "reviews_policy_unconfigured" {
					return nil
				}
				return err
			}
			if policy.Revision < 1 {
				return nil
			}
			var busy bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_selections WHERE project_id=? AND channel='cli' AND state IN('queued','selecting')) OR EXISTS(SELECT 1 FROM review_operations WHERE project_id=? AND channel='cli' AND state IN('preparing','posting','active','uncertain'))`, project, project).Scan(&busy); err != nil {
				return err
			}
			if busy {
				return nil
			}
			basis, candidates, err := automaticPlanningBasisChannel(ctx, tx, project, c, "", "cli")
			if err != nil {
				if errors.Is(err, errAutomaticParentCycle) {
					return nil
				}
				return err
			}
			if !candidates {
				return nil
			}
			var seen bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_automatic_selections WHERE project_id=? AND (basis_sha256=? OR settled_basis_sha256=?))`, project, basis, basis).Scan(&seen); err != nil {
				return err
			}
			if seen {
				worked, err = e.retryAutomaticSourceChannelTx(ctx, tx, project, basis, s.Revision, time.Now(), "cli")
				return err
			}
			// Use the same acceptance implementation as explicit Run, in this exact
			// reauthorized snapshot, without manufacturing a credential or HTTP request.
			receipt, err := e.enqueueRunTx(ctx, tx, scope, RunRequest{Channel: "cli"})
			if err != nil {
				var failure *core.Error
				if errors.As(err, &failure) && (failure.Status == 409 || failure.Code == "overloaded") {
					return nil
				}
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO review_automatic_selections(selection_id,project_id,scheduling_revision,basis_sha256,actor_id,worker_id,credential_id) VALUES(?,?,?,?,?,?,?)`, receipt.ID, project, s.Revision, basis, s.actor, s.AuthorizingWorkerID, s.credential)
			if err == nil {
				worked = true
			}
			return err
		})
		if err != nil {
			return false, err
		}
		if worked {
			return true, nil
		}
	}
	return false, nil
}
