package reviewexec

import (
	"context"
	"database/sql"
	"slices"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// Retained requests may survive pause-only or successor-only configuration
// changes. Every intervening revision must preserve the request's source plan.
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
		if n != expected || len(body) > 8192 || strictjson.Decode([]byte(body), &c) != nil || validateConfigurationShape(c) != nil {
			return false, refreshStorageError()
		}
		expected++
		index := slices.Index(c.Queue, o.PullNumber)
		if index < 0 {
			return false, nil
		}
		if n == o.ConfigurationRevision {
			if c.Paused {
				return false, nil
			}
			original, previous = c, c
			prefix = slices.Clone(c.Queue[:index+1])
			continue
		}
		if c.Paused != previous.Paused {
			if !sameSourceConfiguration(c, previous) {
				return false, nil
			}
			previous = c
			continue
		}
		if c.CapabilityID != original.CapabilityID || c.Mode != original.Mode || !sameChannelSet(c.Channels, original.Channels) || !slices.Equal(c.Queue[:index+1], prefix) || slices.Equal(c.Queue, previous.Queue) {
			return false, nil
		}
		previous = c
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if expected != revision+1 {
		return false, refreshStorageError()
	}
	return true, nil
}
