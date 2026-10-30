package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
)

// A waiting operation keeps its original plan. Changes strictly after its
// target cannot affect its parent/front selection, but a pause, reconfiguration
// or any change through the target still requires a deliberate new request.
// Every intervening revision is checked so a meaningful change cannot be hidden
// by changing the configuration back before admission.
func compatibleQueuedConfiguration(ctx context.Context, tx *sql.Tx, o Operation, current Configuration, revision int64) (bool, error) {
	if current.Paused || revision < o.ConfigurationRevision {
		return false, nil
	}
	if revision == o.ConfigurationRevision {
		return true, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT revision,body_json FROM review_execution_config WHERE project_id=? AND revision BETWEEN ? AND ? ORDER BY revision`, o.ProjectID, o.ConfigurationRevision, revision)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	expected := o.ConfigurationRevision
	var original, previous Configuration
	var prefix []int64
	for rows.Next() {
		var n int64
		var body string
		var c Configuration
		if err := rows.Scan(&n, &body); err != nil {
			return false, err
		}
		if n != expected || json.Unmarshal([]byte(body), &c) != nil {
			return false, conflict()
		}
		expected++
		index := slices.Index(c.Queue, o.PullNumber)
		if index < 0 || c.Paused {
			return false, nil
		}
		if n == o.ConfigurationRevision {
			original = c
			previous = c
			prefix = slices.Clone(c.Queue[:index+1])
			continue
		}
		if c.CapabilityID != original.CapabilityID || c.Mode != original.Mode || !slices.Equal(c.Channels, original.Channels) || !slices.Equal(c.Queue[:index+1], prefix) || slices.Equal(c.Queue, previous.Queue) {
			return false, nil
		}
		previous = c
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return expected == revision+1, nil
}
