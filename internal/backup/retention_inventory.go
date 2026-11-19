package backup

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func validateRetentionMetadata(ctx context.Context, db sqlReader, reviewsVersion int) error {
	rows, err := db.QueryContext(ctx, `SELECT CASE WHEN length(CAST(key AS BLOB))<=128 THEN key END,CASE WHEN length(CAST(value AS BLOB))<=4096 THEN value END FROM metadata WHERE key GLOB 'retention:*'`)
	if err != nil {
		return invalid()
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if rows.Scan(&key, &value) != nil {
			return invalid()
		}
		if key == "retention:last_pass" {
			if !validRetentionReport(value) {
				return invalid()
			}
			continue
		}
		if reviewsVersion < 4 || !strings.HasPrefix(key, core.ReviewHistoryFloorPrefix) || len(key) == len(core.ReviewHistoryFloorPrefix) {
			return invalid()
		}
		floor, err := strconv.ParseInt(value, 10, 64)
		if err != nil || floor < 1 || strconv.FormatInt(floor, 10) != value {
			return invalid()
		}
		// The review graph validator checks owner, floor snapshot, and the
		// absence of gaps in the published history window in one transaction.
	}
	if rows.Err() != nil {
		return invalid()
	}
	return nil
}

func validRetentionReport(value string) bool {
	var r struct {
		StartedAt  string           `json:"started_at"`
		FinishedAt string           `json:"finished_at"`
		DryRun     *bool            `json:"dry_run"`
		Deleted    map[string]int64 `json:"deleted"`
		Error      *string          `json:"error"`
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(value), &fields) != nil || len(fields) != 5 || json.Unmarshal([]byte(value), &r) != nil ||
		r.DryRun == nil || r.Deleted == nil || r.Error == nil {
		return false
	}
	start, err1 := time.Parse(time.RFC3339Nano, r.StartedAt)
	end, err2 := time.Parse(time.RFC3339Nano, r.FinishedAt)
	if err1 != nil || err2 != nil || end.Before(start) {
		return false
	}
	switch *r.Error {
	case "", "review_history_failed", "job_history_failed", "receipts_failed", "diagnostics_failed", "incremental_vacuum_failed":
	default:
		return false
	}
	for k, n := range r.Deleted {
		if n < 0 {
			return false
		}
		switch k {
		case "review_pull_memberships", "review_run_memberships", "review_attempt_memberships", "review_retired_memberships",
			"review_finding_versions", "review_run_versions", "review_attempt_versions", "review_pull_versions", "review_snapshots", "events",
			"job_history", "job_progress_history", "job_summary_history", "note_revisions", "receipts", "review_selection_decision_history":
		default:
			return false
		}
	}
	return true
}
