package reviews

import "strings"

var historyPullRecordV4 = historyRecordSpec{
	versions: "review_pull_versions", memberships: "review_pull_memberships", current: "review_current_pulls",
	keys: []string{"number"}, placement: []string{"position"},
	body: []string{"title", "state", "head_sha", "base_ref", "base_sha", "ci_state", "mergeability", "complete", "run_count", "attempt_count", "recent_runs_json", "latest_attempts_json"},
}

var historyPullRecord = func() historyRecordSpec {
	spec := historyPullRecordV4
	spec.body = append(append([]string{}, spec.body...), "metadata_json")
	return spec
}()

var historyRunRecord = historyRecordSpec{
	versions: "review_run_versions", memberships: "review_run_memberships", current: "review_current_runs",
	keys: []string{"id"}, placement: []string{"pull_number", "run_order"},
	body: []string{"pull_number", "source", "source_run_id", "channel", "head_sha", "reviewer", "scope", "outcome", "attributable", "complete", "started_at", "finished_at", "found", "accepted", "routed", "finalized", "fixed", "coverage_limits_json"},
}

var historyAttemptRecord = historyRecordSpec{
	versions: "review_attempt_versions", memberships: "review_attempt_memberships", current: "review_current_attempts",
	keys: []string{"source", "source_attempt_id"}, placement: []string{"pull_number", "position"},
	body: []string{"pull_number", "channel", "head_sha", "state", "started_at", "finished_at", "duration_seconds", "source_run_id", "trigger_id", "provider_review_id"},
}

var historyRetiredRecord = historyRecordSpec{
	versions: "review_retired_versions", memberships: "review_retired_memberships", current: "review_current_retired",
	keys: []string{"number"}, placement: []string{"position"}, body: []string{"reason"},
}

// Current lookup deliberately starts from the selected snapshot and workspace
// before visiting its indexed current references. The historical branch tests
// its snapshot guard before walking any interval history. CROSS JOIN preserves
// that guard-first order even when a project accumulates many old versions.
func historyReadView(name string, spec historyRecordSpec, columns []string) string {
	contains := func(names []string, name string) bool {
		for _, candidate := range names {
			if candidate == name {
				return true
			}
		}
		return false
	}
	selection := func(alias string) string {
		parts := make([]string, 0, len(columns))
		for _, column := range columns {
			expression := "v." + column
			switch column {
			case "project_id", "snapshot_id":
				expression = "s." + column
			case "snapshot_revision":
				expression = "s.revision"
			default:
				if contains(spec.keys, column) || contains(spec.placement, column) {
					expression = alias + "." + column
				}
			}
			parts = append(parts, expression+" AS "+column)
		}
		return strings.Join(parts, ",")
	}
	joins := func(left, right string, columns []string) string {
		parts := []string{left + ".project_id=" + right + ".project_id"}
		for _, column := range columns {
			parts = append(parts, left+"."+column+"="+right+"."+column)
		}
		return strings.Join(parts, " AND ")
	}
	currentKeys := append([]string{}, spec.keys...)
	currentKeys = append(currentKeys, "start_revision")
	currentKeys = append(currentKeys, spec.placement...)
	versionKeys := append([]string{}, spec.keys...)
	versionKeys = append(versionKeys, "version_revision")
	if contains(spec.placement, "pull_number") {
		versionKeys = append(versionKeys, "pull_number")
	}
	return "CREATE VIEW " + name + " AS SELECT " + selection("c") +
		" FROM review_snapshots s CROSS JOIN review_workspace w ON w.project_id=s.project_id AND w.snapshot_id=s.snapshot_id" +
		" CROSS JOIN " + spec.current + " c ON c.project_id=s.project_id" +
		" CROSS JOIN " + spec.memberships + " m ON " + joins("c", "m", currentKeys) +
		" CROSS JOIN " + spec.versions + " v ON " + joins("m", "v", versionKeys) +
		" UNION ALL SELECT " + selection("m") +
		" FROM review_snapshots s CROSS JOIN " + spec.memberships + " m ON m.project_id=s.project_id AND m.start_revision<=s.revision AND (m.end_revision IS NULL OR s.revision<m.end_revision)" +
		" CROSS JOIN " + spec.versions + " v ON " + joins("m", "v", versionKeys) +
		" WHERE NOT EXISTS(SELECT 1 FROM review_workspace w WHERE w.project_id=s.project_id AND w.snapshot_id=s.snapshot_id);"
}

// HistoryReadViewSQL returns independent canonical definitions for startup and
// read-only backup inspection. These are read views, never writable adapters.
func HistoryReadViewSQL() map[string]string { return historyReadViewSQL(true) }

// HistoryReadViewSQLV6 preserves the exact predecessor backup/startup definition.
func HistoryReadViewSQLV6() map[string]string { return historyReadViewSQL(false) }
func historyReadViewSQL(metadata bool) map[string]string {
	result :=
		map[string]string{
			"review_pulls":         historyReadView("review_pulls", historyPullRecord, []string{"project_id", "snapshot_id", "position", "number", "title", "state", "head_sha", "base_ref", "base_sha", "ci_state", "mergeability", "complete", "run_count", "attempt_count", "recent_runs_json", "latest_attempts_json"}),
			"review_runs":          historyReadView("review_runs", historyRunRecord, []string{"project_id", "snapshot_id", "pull_number", "run_order", "id", "source", "source_run_id", "channel", "head_sha", "reviewer", "scope", "outcome", "attributable", "complete", "started_at", "finished_at", "found", "accepted", "routed", "finalized", "fixed", "coverage_limits_json", "snapshot_revision", "version_revision"}),
			"review_attempts":      historyReadView("review_attempts", historyAttemptRecord, []string{"project_id", "snapshot_id", "pull_number", "position", "source", "source_attempt_id", "channel", "head_sha", "state", "started_at", "finished_at", "duration_seconds", "source_run_id", "trigger_id", "provider_review_id"}),
			"review_retired_pulls": historyReadView("review_retired_pulls", historyRetiredRecord, []string{"project_id", "snapshot_id", "position", "number", "reason"}),
			"review_findings":      `CREATE VIEW review_findings AS SELECT r.project_id,r.snapshot_id,r.pull_number,r.id AS run_id,f.position,f.source_finding_id,f.title,f.detail,f.severity,f.disposition,f.decision_reason,f.actor,f.decided_at,f.fix_sha,f.proof_note,f.resolved_at,f.route_json FROM review_runs r JOIN review_finding_versions f ON f.project_id=r.project_id AND f.run_id=r.id AND f.run_version_revision=r.version_revision AND f.pull_number=r.pull_number;`,
		}
	if metadata {
		columns := append([]string{"project_id", "snapshot_id", "position", "number"}, historyPullRecord.body...)
		result["review_pulls"] = historyReadView("review_pulls", historyPullRecord, columns)
	}
	return result
}
