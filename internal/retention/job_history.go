package retention

import (
	"context"
	"fmt"
	"time"
)

const retainedJobHistoryRevisions = 20

type jobHistoryPolicy struct {
	table, owner, revision, timestamp string
}

var jobHistoryPolicies = []jobHistoryPolicy{
	{"job_history", "job_id", "revision", "created_at"},
	{"job_progress_history", "job_id", "job_revision", "created_at"},
	{"job_summary_history", "job_id", "revision", "created_at"},
	{"note_revisions", "note_id", "revision", "recorded_at"},
}

func pruneJobHistory(ctx context.Context, p *pass, cutoff time.Time) error {
	for _, spec := range jobHistoryPolicies {
		if err := p.delete(ctx, spec.table, spec.where(), cutoff.Unix(), cutoff.Nanosecond()); err != nil {
			return fmt.Errorf("prune %s: %w", spec.table, err)
		}
	}
	return nil
}

func historyTimeTuple(at string) string {
	// Compare seconds and nanoseconds separately: RFC3339Nano optional fractions
	// are not lexicographically ordered, and SQLite's date functions round
	// them. Strip the fractional component before converting the whole seconds.
	zoneLength := "CASE WHEN substr(" + at + ",-1)='Z' THEN 1 ELSE 6 END"
	seconds := "unixepoch(substr(" + at + ",1,19)||substr(" + at + ",-(" + zoneLength + ")))"
	fraction := "CASE WHEN substr(" + at + ",20,1)='.' THEN substr(" + at + ",21,length(" + at + ")-20-(" + zoneLength + ")) ELSE '' END"
	nanos := "CAST(substr((" + fraction + ")||'000000000',1,9) AS INTEGER)"
	return "(" + seconds + "," + nanos + ")"
}

func (s jobHistoryPolicy) where() string {
	// Progress may contain many entries per owning revision. DENSE_RANK
	// assigns one rank to every row in a revision; windows also detect
	// recent peers and the current projection evidence without rescanning
	// a large owner's history separately for each of its revisions.
	if s.table == "job_progress_history" {
		return fmt.Sprintf(`rowid IN (
			WITH ranked AS (
				SELECT sequence,job_revision,
					DENSE_RANK() OVER (PARTITION BY project_id,job_id ORDER BY job_revision DESC) AS rank,
					MAX(sequence) OVER (PARTITION BY project_id,job_id) AS current_sequence,
					MAX(sequence) OVER (PARTITION BY project_id,job_id,job_revision) AS last_sequence,
					MAX(CASE WHEN %s<(?,?) THEN 0 ELSE 1 END)
						OVER (PARTITION BY project_id,job_id,job_revision) AS protect_window
				FROM job_progress_history
			)
			SELECT sequence FROM ranked
			WHERE rank>%d AND job_revision>1 AND protect_window=0 AND last_sequence<>current_sequence
		)`, historyTimeTuple("created_at"), retainedJobHistoryRevisions)
	}

	// The other archives contain a single row per owning revision.
	return fmt.Sprintf(
		"%s<(?,?) AND %s.%s>1 AND EXISTS(SELECT 1 FROM %s recent WHERE recent.project_id=%s.project_id AND recent.%s=%s.%s AND recent.%s>%s.%s ORDER BY recent.%s DESC LIMIT 1 OFFSET %d)",
		historyTimeTuple(s.table+"."+s.timestamp), s.table, s.revision, s.table, s.table, s.owner, s.table, s.owner, s.revision, s.table, s.revision, s.revision, retainedJobHistoryRevisions-1,
	)
}
