package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
)

func (e *Engine) generateAutomaticCLI(ctx context.Context, readers map[string]TargetReader) (bool, error) {
	var projects []string
	if err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT project_id FROM review_scheduling WHERE automatic_cli=1 ORDER BY (SELECT lower(repository) FROM projects WHERE id=project_id)<>?,(project_id<=?),project_id LIMIT 100`, preferredCLIRepository, e.automaticCLICursor)
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
	// Visit the bounded project set even after creating a preferred selection.
	// Account-level preference is enforced again at claim; independent accounts
	// must not starve simply because preferred work is always available.
	generated := false
	selectedAccounts := map[string]bool{}
	for _, project := range projects {
		e.automaticCLICursor = project
		var account string
		err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
			c, _, err := readConfiguration(ctx, tx, project)
			if err != nil {
				var failure *core.Error
				if errors.As(err, &failure) && failure.Code == "reviews_unconfigured" {
					return nil
				}
				return err
			}
			capability, ok := e.capabilities[c.CapabilityID]
			if ok {
				account = capability.Provider + "\x00" + capability.Account
			}
			return nil
		})
		if automaticProjectRefused(err) || isStoredUnavailable(err, storedConfigurationHistory) {
			continue
		}
		if err != nil {
			return generated, err
		}
		if selectedAccounts[account] {
			continue
		}
		worked, err := e.generateAutomaticProject(ctx, project, "cli", readers)
		if automaticProjectRefused(err) {
			continue
		}
		if err != nil {
			return generated, err
		}
		if worked {
			generated = true
			selectedAccounts[account] = true
		}
	}
	return generated, nil
}
