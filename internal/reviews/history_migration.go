package reviews

import "strings"

// historyMigrationSQL is an exclusive-startup schema group. The core schema
// owner supplies its transaction and metadata update; this function neither
// advances a version nor changes the production startup migration list.
func historyMigrationSQL() []string {
	const assertion = "review_history_migration_assert"
	check := func(query string) string {
		return "INSERT INTO temp." + assertion + "(ok) SELECT NOT EXISTS(" + query + ");"
	}
	statements := []string{
		"CREATE TEMP TABLE " + assertion + "(ok INTEGER NOT NULL CHECK(ok=1));",
		check("SELECT 1 FROM pragma_foreign_key_check"),
		check(`SELECT 1 FROM review_runs r LEFT JOIN review_snapshots s ON s.project_id=r.project_id AND s.snapshot_id=r.snapshot_id WHERE r.snapshot_revision IS NOT s.revision`),
		check(`SELECT 1 FROM review_workspace w LEFT JOIN review_snapshots s ON s.project_id=w.project_id AND s.snapshot_id=w.snapshot_id WHERE s.project_id IS NULL OR s.revision!=w.revision`),
		check(`SELECT 1 FROM review_snapshots s LEFT JOIN review_workspace w ON w.project_id=s.project_id WHERE w.project_id IS NULL OR s.revision>w.revision`),
	}
	records := []struct {
		legacy string
		spec   historyRecordSpec
	}{
		{"legacy_review_pulls", historyPullRecordV4},
		{"legacy_review_runs", historyRunRecord},
		{"legacy_review_attempts", historyAttemptRecord},
		{"legacy_review_retired_pulls", historyRetiredRecord},
	}
	// Renaming keeps all old references intact. Only empty/new physical objects
	// are created before every legacy body has been copied and checked.
	for _, table := range []string{"review_findings", "review_runs", "review_attempts", "review_pulls", "review_retired_pulls"} {
		statements = append(statements, "ALTER TABLE "+table+" RENAME TO legacy_"+table+";")
	}
	statements = append(statements, "DROP TRIGGER review_runs_snapshot_revision_insert;", "DROP TRIGGER review_runs_snapshot_revision_update;")
	statements = append(statements, HistorySchemaSQLV6()...)
	qualify := func(alias string, columns []string) string {
		out := make([]string, len(columns))
		for i, column := range columns {
			out[i] = alias + "." + column
		}
		return strings.Join(out, ",")
	}
	for _, record := range records {
		columns := append([]string{"project_id"}, record.spec.keys...)
		columns = append(columns, "version_revision")
		columns = append(columns, record.spec.body...)
		selected := []string{"l.project_id", qualify("l", record.spec.keys), "s.revision", qualify("l", record.spec.body)}
		statements = append(statements, "INSERT INTO "+record.spec.versions+"("+strings.Join(columns, ",")+") SELECT "+strings.Join(selected, ",")+" FROM "+record.legacy+" l JOIN review_snapshots s ON s.project_id=l.project_id AND s.snapshot_id=l.snapshot_id ORDER BY l.project_id,s.revision,"+qualify("l", record.spec.placement)+";")
	}
	statements = append(statements, `INSERT INTO review_finding_versions(project_id,run_id,run_version_revision,pull_number,position,source_finding_id,title,detail,severity,disposition,decision_reason,actor,decided_at,fix_sha,proof_note,resolved_at,route_json)
 SELECT f.project_id,f.run_id,r.snapshot_revision,f.pull_number,f.position,f.source_finding_id,f.title,f.detail,f.severity,f.disposition,f.decision_reason,f.actor,f.decided_at,f.fix_sha,f.proof_note,f.resolved_at,f.route_json
 FROM legacy_review_findings f JOIN legacy_review_runs r ON r.project_id=f.project_id AND r.snapshot_id=f.snapshot_id AND r.pull_number=f.pull_number AND r.id=f.run_id
 ORDER BY f.project_id,r.snapshot_revision,f.pull_number,r.run_order,f.position;`)
	for _, record := range records {
		columns := append([]string{"project_id"}, record.spec.keys...)
		columns = append(columns, "start_revision", "end_revision", "version_revision")
		columns = append(columns, record.spec.placement...)
		// The next project snapshot closes visibility even when this identity is
		// omitted there. LEAD over this record's appearances would erase that gap.
		statements = append(statements, `WITH boundaries AS (SELECT project_id,snapshot_id,revision,LEAD(revision) OVER(PARTITION BY project_id ORDER BY revision) AS next_revision FROM review_snapshots)
 INSERT INTO `+record.spec.memberships+"("+strings.Join(columns, ",")+") SELECT l.project_id,"+qualify("l", record.spec.keys)+",s.revision,s.next_revision,s.revision,"+qualify("l", record.spec.placement)+" FROM "+record.legacy+" l JOIN boundaries s ON s.project_id=l.project_id AND s.snapshot_id=l.snapshot_id ORDER BY l.project_id,s.revision,"+qualify("l", record.spec.placement)+";")
		currentColumns := append([]string{"project_id"}, record.spec.keys...)
		currentColumns = append(currentColumns, "start_revision")
		currentColumns = append(currentColumns, record.spec.placement...)
		statements = append(statements, "INSERT INTO "+record.spec.current+"("+strings.Join(currentColumns, ",")+") SELECT "+qualify("m", currentColumns)+" FROM "+record.spec.memberships+" m JOIN review_workspace w ON w.project_id=m.project_id AND w.revision=m.start_revision;")
	}
	for _, table := range []string{"legacy_review_findings", "legacy_review_runs", "legacy_review_attempts", "legacy_review_pulls", "legacy_review_retired_pulls"} {
		statements = append(statements, "DROP TABLE "+table+";")
	}
	views := HistoryReadViewSQLV6()
	for _, name := range []string{"review_pulls", "review_runs", "review_findings", "review_attempts", "review_retired_pulls"} {
		statements = append(statements, views[name])
	}
	for _, query := range historyReferenceQueries() {
		statements = append(statements, check(query))
	}
	statements = append(statements, check("SELECT 1 FROM pragma_foreign_key_check"), "DROP TABLE temp."+assertion+";")
	return statements
}
