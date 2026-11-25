package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
)

const automaticCLITraversalLimit = 100

// Each priority group has independent wraparound and a reserved part of the
// bounded traversal. An idle group lends its unused slots to the other one.
func automaticCLIProjects(ctx context.Context, tx *sql.Tx, preferred bool, cursor string, limit int) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT s.project_id FROM review_scheduling s LEFT JOIN projects p ON p.id=s.project_id
		WHERE s.automatic_cli=1 AND COALESCE(lower(p.repository)=?,0)=?
		ORDER BY (s.project_id<=?),s.project_id LIMIT ?`, preferredCLIRepository, preferred, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var projects []string
	for rows.Next() {
		var project string
		if err = rows.Scan(&project); err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return projects, rows.Close()
}

func (e *Engine) automaticCLIAccount(ctx context.Context, project string) (string, error) {
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
			account = automaticAccountKey(capability)
		}
		return nil
	})
	return account, err
}

func (e *Engine) generateAutomaticCLI(ctx context.Context, readers map[string]TargetReader) (bool, error) {
	return e.generateAutomaticCLIWithAccountLookup(ctx, readers, e.automaticCLIAccount)
}

func (e *Engine) generateAutomaticCLIWithAccountLookup(ctx context.Context, readers map[string]TargetReader, lookupAccount func(context.Context, string) (string, error)) (bool, error) {
	var preferred, other []string
	if err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		const half = automaticCLITraversalLimit / 2
		var err error
		preferred, err = automaticCLIProjects(ctx, tx, true, e.automaticCLICursor, half)
		if err != nil {
			return err
		}
		other, err = automaticCLIProjects(ctx, tx, false, e.automaticCLIOtherCursor, automaticCLITraversalLimit-len(preferred))
		if err != nil {
			return err
		}
		if len(preferred) == half && len(other) < half {
			preferred, err = automaticCLIProjects(ctx, tx, true, e.automaticCLICursor, automaticCLITraversalLimit-len(other))
		}
		return err
	}); err != nil {
		return false, err
	}
	// Visit the bounded project set even after creating a preferred selection.
	// Account-level preference is enforced again at claim; independent accounts
	// must not starve simply because preferred work is always available.
	generated := false
	selectedAccounts := map[string]bool{}
	for i, project := range append(preferred, other...) {
		if i < len(preferred) {
			e.automaticCLICursor = project
		} else {
			e.automaticCLIOtherCursor = project
		}
		account, err := lookupAccount(ctx, project)
		if automaticProjectRefused(err) || isStoredUnavailable(err, storedConfigurationHistory) {
			continue
		}
		if err != nil {
			var failure *core.Error
			if errors.As(err, &failure) && failure.Code == "overloaded" && ctx.Err() == nil {
				continue // Bounded local lookup; no account turn was consumed.
			}
			return generated, err
		}
		if account == "" || selectedAccounts[account] {
			continue
		}
		worked, err := e.generateAutomaticProjectForAccount(ctx, project, "cli", readers, account)
		if automaticProjectRefused(err) {
			continue
		}
		if err != nil {
			var failure *core.Error
			if errors.As(err, &failure) && failure.Code == "overloaded" && ctx.Err() == nil {
				// No intent was accepted. Other projects on the same
				// account can still make progress during this bounded pass.
				continue
			}
			return generated, err
		}
		if worked {
			generated = true
			selectedAccounts[account] = true
		}
	}
	return generated, nil
}
