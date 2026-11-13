package reviewexec

import (
	"context"
	"database/sql"
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
		worked, err := e.generateAutomaticProject(ctx, project, "cli", readers)
		if automaticProjectRefused(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if worked {
			return true, nil
		}
	}
	return false, nil
}
