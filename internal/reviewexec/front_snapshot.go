package reviewexec

import (
	"context"
	"fmt"
	"strings"
)

// ValidateFrontSnapshot validates historical source facts, not current policy,
// rounds, credentials or repository binding. Old valid frames remain backups.
func ValidateFrontSnapshot(ctx context.Context, q EvidenceQuery) error {
	if err := validateFrontPhysical(ctx, q); err != nil {
		return err
	}
	project, channel := "", ""
	for {
		rows, err := q.QueryContext(ctx, `SELECT project_id,channel FROM review_front_frames WHERE project_id>? OR (project_id=? AND channel>?) ORDER BY project_id,channel LIMIT 64`, project, project, channel)
		if err != nil {
			return err
		}
		type key struct{ project, channel string }
		page := []key{}
		for rows.Next() {
			var k key
			if err = rows.Scan(&k.project, &k.channel); err != nil {
				break
			}
			page = append(page, k)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, k := range page {
			if !identifier.MatchString(k.project) || !validChannel(k.channel) {
				return runUnavailable()
			}
			if _, found, err := readFrontFrame(ctx, q, k.project, k.channel); err != nil {
				return err
			} else if !found {
				return runUnavailable()
			}
			project, channel = k.project, k.channel
		}
		if len(page) < 64 {
			return nil
		}
	}
}
func validateFrontPhysical(ctx context.Context, q EvidenceQuery) error {
	rows, err := q.QueryContext(ctx, `SELECT name,CASE WHEN length(CAST(sql AS BLOB))<=16384 THEN sql END FROM sqlite_schema WHERE tbl_name='review_front_frames' AND (type='table' OR sql IS NOT NULL) LIMIT 3`)
	if err != nil {
		return err
	}
	count := 0
	for rows.Next() {
		var name, body string
		if err = rows.Scan(&name, &body); err != nil {
			break
		}
		count++
		if name != "review_front_frames" || strings.TrimSpace(body) != strings.TrimSpace(FrontSchemaSQL()[0]) {
			err = runUnavailable()
			break
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if count != 1 {
		return runUnavailable()
	}
	checks := []string{"project_id=''", "repository_id<1", "configuration_revision<1", "complete NOT IN (0,1)", "NOT EXISTS(SELECT 1 FROM projects p WHERE p.id=review_front_frames.project_id)"}
	for _, col := range strings.Fields("repository_id configuration_revision complete") {
		checks = append(checks, "typeof("+col+")!='integer'")
	}
	for _, col := range strings.Fields("project_id channel checked_at source_observed_at source_reason prefix_json") {
		limit := 128
		if col == "checked_at" || col == "source_observed_at" {
			limit = 64
		}
		if col == "prefix_json" {
			limit = maxFrontPrefixBytes
		}
		checks = append(checks, fmt.Sprintf("typeof(%s)!='text' OR length(CAST(%s AS BLOB))>%d", col, col, limit))
	}
	var bad bool
	if err = q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM review_front_frames WHERE "+strings.Join(checks, " OR ")+")").Scan(&bad); err != nil {
		return err
	}
	if bad {
		return runUnavailable()
	}
	return nil
}
