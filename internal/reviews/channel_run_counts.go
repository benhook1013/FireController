package reviews

import (
	"context"
	"database/sql"
	"strings"
)

// ChannelRunCounts counts every retained run visible in the selected snapshot,
// independently of preview pages, outcomes, attempts or native credit.
type ChannelRunCounts struct {
	Hosted   int64 `json:"hosted"`
	CLI      int64 `json:"cli"`
	Subagent int64 `json:"subagent"`
	Manual   int64 `json:"manual"`
}

// Valid uses subtraction so malformed large counters cannot overflow a sum.
func (c ChannelRunCounts) Valid(total int64) bool {
	if total < 0 {
		return false
	}
	for _, n := range []int64{c.Hosted, c.CLI, c.Subagent, c.Manual} {
		if n < 0 || n > total {
			return false
		}
		total -= n
	}
	return total == 0
}
func (c *ChannelRunCounts) add(channel string, delta int64) error {
	switch channel {
	case "hosted":
		c.Hosted += delta
	case "cli":
		c.CLI += delta
	case "subagent":
		c.Subagent += delta
	case "manual":
		c.Manual += delta
	default:
		return historyInconsistent()
	}
	return nil
}

const channelRunCountColumns = "hosted_run_count,cli_run_count,subagent_run_count,manual_run_count"

var channelRunCountNames = []string{"hosted_run_count", "cli_run_count", "subagent_run_count", "manual_run_count"}

func channelRunCountsMigrationSQL() []string {
	var out []string
	for _, name := range channelRunCountNames {
		out = append(out, "ALTER TABLE review_current_totals ADD COLUMN "+name+" INTEGER NOT NULL DEFAULT 0 CHECK("+name+">=0)")
	}
	return out
}

// CurrentSummarySchemaSQL defines schema nine. Older bundles retain their exact
// canonical declarations through CurrentSummarySchemaSQLV8.
func CurrentSummarySchemaSQL() []string {
	out := CurrentSummarySchemaSQLV8()
	var fields []string
	for _, name := range channelRunCountNames {
		fields = append(fields, name+" INTEGER NOT NULL DEFAULT 0 CHECK("+name+">=0)")
	}
	out[0] = strings.Replace(out[0], ",\n PRIMARY KEY(project_id,pull_number)", ", "+strings.Join(fields, ", ")+",\n PRIMARY KEY(project_id,pull_number)", 1)
	return out
}

const channelRunCountAggregate = `SELECT coalesce(sum(v.channel='hosted'),0),coalesce(sum(v.channel='cli'),0),coalesce(sum(v.channel='subagent'),0),coalesce(sum(v.channel='manual'),0)
 FROM review_current_runs r
 CROSS JOIN review_run_memberships m ON m.project_id=r.project_id AND m.id=r.id AND m.start_revision=r.start_revision AND m.pull_number=r.pull_number AND m.run_order=r.run_order
 CROSS JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision AND v.pull_number=m.pull_number
 WHERE r.project_id=t.project_id AND r.pull_number=t.pull_number`

func backfillChannelRunCounts(ctx context.Context, q currentSummarySQL) error {
	var version int
	if err := q.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&version); err != nil {
		return err
	}
	if version < 9 {
		return nil
	}
	_, err := q.ExecContext(ctx, `UPDATE review_current_totals AS t SET (`+channelRunCountColumns+`)=(`+channelRunCountAggregate+`)`)
	return err
}
func validateChannelRunCounts(ctx context.Context, q HistoryQuery) error {
	var bad bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_current_totals t WHERE typeof(hosted_run_count)!='integer' OR typeof(cli_run_count)!='integer' OR typeof(subagent_run_count)!='integer' OR typeof(manual_run_count)!='integer' OR hosted_run_count<0 OR cli_run_count<0 OR subagent_run_count<0 OR manual_run_count<0 OR run_count!=hosted_run_count+cli_run_count+subagent_run_count+manual_run_count OR (`+channelRunCountColumns+`)!=(`+channelRunCountAggregate+`))`).Scan(&bad)
	if err != nil {
		return err
	}
	if bad {
		return historyInconsistent()
	}
	return nil
}

// The workspace check and both branches run inside the caller's captured read
// transaction. Mutable current caches never label a pinned historical response.
func fillChannelRunCounts(ctx context.Context, tx *sql.Tx, project string, meta snapshotMeta, pull *PullSummary) error {
	var current bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_workspace WHERE project_id=? AND snapshot_id=? AND revision=?)`, project, meta.SnapshotID, meta.Revision).Scan(&current); err != nil {
		return err
	}
	var counts ChannelRunCounts
	if current {
		if err := tx.QueryRowContext(ctx, `SELECT `+channelRunCountColumns+` FROM review_current_totals WHERE project_id=? AND pull_number=?`, project, pull.Number).Scan(&counts.Hosted, &counts.CLI, &counts.Subagent, &counts.Manual); err != nil {
			return err
		}
	} else {
		rows, err := tx.QueryContext(ctx, historicalChannelRunCountsSQL, project, pull.Number, meta.Revision, meta.Revision)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var channel string
			var count int64
			if err = rows.Scan(&channel, &count); err != nil {
				return err
			}
			if err = counts.add(channel, count); err != nil {
				return err
			}
		}
		if err = rows.Err(); err != nil {
			return err
		}
	}
	if !counts.Valid(pull.RunCount) {
		return historyInconsistent()
	}
	pull.RunCounts = counts
	return nil
}

const historicalChannelRunCountsSQL = `SELECT v.channel,count(*)
 FROM review_run_memberships m INDEXED BY review_run_memberships_page
 CROSS JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision AND v.pull_number=m.pull_number
 WHERE m.project_id=? AND m.pull_number=? AND m.start_revision<=? AND (m.end_revision IS NULL OR ?<m.end_revision)
 GROUP BY v.channel`
