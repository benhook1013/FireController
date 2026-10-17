package reviews

import (
	"fmt"
	"strings"
	"testing"
)

func historyPhysicalFixture(t *testing.T) *apiFixture {
	t.Helper()
	f := newAPIFixture(t)
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(t.Context(), `INSERT INTO projects(id,name,github_repository_id,repository) VALUES('history-other','Other',702,'owner/other')`); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{f.project, "history-other"} {
		revisions := []int{1, 2, 3, 4, 5}
		if project == "history-other" {
			revisions = []int{9}
		}
		for _, revision := range revisions {
			if _, err = tx.ExecContext(t.Context(), `INSERT INTO review_snapshots(project_id,snapshot_id,revision,repository_id,source,observed_at,recorded_at,payload_sha256) VALUES(?,?,?,701,'synthetic-source','','','')`, project, fmt.Sprintf("history-%s-%d", project, revision), revision); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, number := range []int{1, 2} {
		historyPhysicalExec(t, f, `INSERT INTO review_pull_versions VALUES(?,?,1,'Retained','','','','','','',1,0,0,'[]','[]')`, f.project, number)
	}
	historyPhysicalExec(t, f, `INSERT INTO review_pull_versions VALUES(?,1,3,'Changed','','','','','','',1,0,0,'[]','[]')`, f.project)
	for _, run := range []struct {
		id, source    string
		version, pull int
	}{{"run-one", "producer-a", 1, 1}, {"run-one", "producer-a", 3, 2}, {"run-two", "producer-b", 1, 1}} {
		historyPhysicalExec(t, f, `INSERT INTO review_run_versions VALUES(?,?,?,?,?,'shared-source-id','cli','','','','completed',1,1,'','',1,1,0,1,0,'[]')`, f.project, run.id, run.version, run.pull, run.source)
	}
	for _, attempt := range []struct {
		source        string
		version, pull int
	}{{"producer-a", 1, 1}, {"producer-a", 3, 2}, {"producer-b", 1, 1}} {
		historyPhysicalExec(t, f, `INSERT INTO review_attempt_versions VALUES(?,?,'shared-attempt',?,?,'cli','','completed','','',NULL,'shared-source-id','','')`, f.project, attempt.source, attempt.version, attempt.pull)
	}
	for _, number := range []int{8, 9} {
		historyPhysicalExec(t, f, `INSERT INTO review_retired_versions VALUES(?,?,1,'defunct')`, f.project, number)
	}
	historyPhysicalExec(t, f, `INSERT INTO review_retired_versions VALUES(?,8,3,'superseded')`, f.project)
	for _, version := range []int{1, 3} {
		pull := 1
		if version == 3 {
			pull = 2
		}
		historyPhysicalExec(t, f, `INSERT INTO review_finding_versions VALUES(?,'run-one',?,?,0,'finding-one','Title',?,'medium','accepted','','','','','','','null')`, f.project, version, pull, fmt.Sprintf("Detail at version %d", version))
	}
	return f
}

func historyPhysicalExec(t *testing.T, f *apiFixture, statement string, args ...any) {
	t.Helper()
	if _, err := f.store.DB().ExecContext(t.Context(), statement, args...); err != nil {
		t.Fatalf("fixture SQL: %v\n%s", err, statement)
	}
}
func historyPhysicalReject(t *testing.T, f *apiFixture, statement string, args ...any) {
	t.Helper()
	if _, err := f.store.DB().ExecContext(t.Context(), statement, args...); err == nil {
		t.Fatalf("invalid reference accepted:\n%s", statement)
	}
}

func historyPhysicalMemberships(t *testing.T, f *apiFixture) {
	t.Helper()
	for _, statement := range []string{
		`INSERT INTO review_pull_memberships VALUES(?,1,1,NULL,1,0)`,
		`INSERT INTO review_run_memberships VALUES(?,'run-one',1,NULL,1,1,0)`,
		`INSERT INTO review_attempt_memberships VALUES(?,'producer-a','shared-attempt',1,NULL,1,1,0)`,
		`INSERT INTO review_retired_memberships VALUES(?,8,1,NULL,1,0)`,
		`INSERT INTO review_current_pulls VALUES(?,1,1,0)`,
		`INSERT INTO review_current_runs VALUES(?,'run-one',1,1,0)`,
		`INSERT INTO review_current_attempts VALUES(?,'producer-a','shared-attempt',1,1,0)`,
		`INSERT INTO review_current_retired VALUES(?,8,1,0)`,
	} {
		historyPhysicalExec(t, f, statement, f.project)
	}
}

func TestHistoryPhysicalSchemaExactVersionsAndHistoricalMembership(t *testing.T) {
	f := historyPhysicalFixture(t)
	historyPhysicalMemberships(t, f)
	var schema string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&schema); err != nil || schema != "4" {
		t.Fatal("physical declarations changed legacy schema version", schema, err)
	}
	var tables int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='table' AND name IN ('review_pull_versions','review_run_versions','review_finding_versions','review_attempt_versions','review_retired_versions','review_pull_memberships','review_run_memberships','review_attempt_memberships','review_retired_memberships','review_current_pulls','review_current_runs','review_current_attempts','review_current_retired')`).Scan(&tables); err != nil || tables != 13 {
		t.Fatal("physical tables missing", tables, err)
	}
	// An omission closes membership but does not erase or rewrite its payload.
	for _, table := range []string{"review_pull_memberships", "review_run_memberships", "review_attempt_memberships", "review_retired_memberships"} {
		historyPhysicalExec(t, f, `UPDATE `+table+` SET end_revision=2 WHERE project_id=?`, f.project)
	}
	for _, statement := range []string{
		`INSERT INTO review_pull_memberships VALUES(?,1,4,NULL,1,7)`,
		`INSERT INTO review_run_memberships VALUES(?,'run-one',4,NULL,3,2,7)`,
		`INSERT INTO review_attempt_memberships VALUES(?,'producer-a','shared-attempt',4,NULL,3,2,7)`,
		`INSERT INTO review_retired_memberships VALUES(?,8,4,NULL,1,7)`,
	} {
		historyPhysicalExec(t, f, statement, f.project)
	}
	// Existing current references still resolve their exact old membership,
	// independently of the newer prospective intervals staged for publication.
	var version, pull, order int
	if err := f.store.DB().QueryRow(`SELECT m.version_revision,m.pull_number,m.run_order FROM review_current_runs c JOIN review_run_memberships m USING(project_id,id,start_revision,pull_number,run_order) WHERE c.project_id=?`, f.project).Scan(&version, &pull, &order); err != nil || version != 1 || pull != 1 || order != 0 {
		t.Fatal("staging changed old current reference", version, pull, order, err)
	}
	for _, test := range []struct{ revision, want int }{{1, 1}, {2, 0}, {3, 0}, {4, 1}} {
		var visible int
		if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_run_memberships WHERE project_id=? AND id='run-one' AND start_revision<=? AND (end_revision IS NULL OR ?<end_revision)`, f.project, test.revision, test.revision).Scan(&visible); err != nil || visible != test.want {
			t.Fatal("gap or return visibility lost", test, visible, err)
		}
	}
	historyPhysicalExec(t, f, `UPDATE review_current_runs SET start_revision=4,pull_number=2,run_order=7 WHERE project_id=?`, f.project)
	for _, test := range []struct {
		version, pull int
		detail        string
	}{{1, 1, "Detail at version 1"}, {3, 2, "Detail at version 3"}} {
		var detail string
		err := f.store.DB().QueryRow(`SELECT f.detail FROM review_run_versions r JOIN review_finding_versions f ON f.project_id=r.project_id AND f.run_id=r.id AND f.run_version_revision=r.version_revision AND f.pull_number=r.pull_number WHERE r.project_id=? AND r.id='run-one' AND r.version_revision=? AND r.pull_number=?`, f.project, test.version, test.pull).Scan(&detail)
		if err != nil || detail != test.detail {
			t.Fatal("exact historical finding version lost", detail, err)
		}
	}
	rows, err := f.store.DB().Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() || rows.Err() != nil {
		t.Fatal("physical foreign key violation", rows.Err())
	}
}

func TestHistoryPhysicalSchemaRejectsForeignAndFutureReferences(t *testing.T) {
	f := historyPhysicalFixture(t)
	tests := []struct {
		name, sql string
		args      []any
	}{
		{"version-owner-project", `INSERT INTO review_pull_versions SELECT 'history-other',number,version_revision,title,state,head_sha,base_ref,base_sha,ci_state,mergeability,complete,run_count,attempt_count,recent_runs_json,latest_attempts_json FROM review_pull_versions WHERE project_id=? AND number=1 AND version_revision=1`, []any{f.project}},
		{"version-owning-snapshot", `INSERT INTO review_retired_versions VALUES(?,10,9,'defunct')`, []any{f.project}},
		{"finding-owner-pr", `UPDATE review_finding_versions SET pull_number=2 WHERE project_id=? AND run_id='run-one' AND run_version_revision=1`, []any{f.project}},
		{"finding-exact-version", `UPDATE review_finding_versions SET run_version_revision=2 WHERE project_id=? AND run_id='run-one' AND run_version_revision=1`, []any{f.project}},
		{"finding-project", `UPDATE review_finding_versions SET project_id='history-other' WHERE project_id=? AND run_version_revision=1`, []any{f.project}},
	}
	for _, table := range []string{"review_pull_memberships", "review_run_memberships", "review_attempt_memberships", "review_retired_memberships"} {
		values := map[string]string{"review_pull_memberships": "?,1,2,NULL,3,0", "review_run_memberships": "?,'run-one',2,NULL,3,2,0", "review_attempt_memberships": "?,'producer-a','shared-attempt',2,NULL,3,2,0", "review_retired_memberships": "?,8,2,NULL,3,0"}[table]
		tests = append(tests, struct {
			name, sql string
			args      []any
		}{table + "-forward-version", `INSERT INTO ` + table + ` VALUES(` + values + `)`, []any{f.project}})
	}
	tests = append(tests,
		struct {
			name, sql string
			args      []any
		}{"run-owner-pr", `INSERT INTO review_run_memberships VALUES(?,'run-one',1,NULL,1,2,0)`, []any{f.project}},
		struct {
			name, sql string
			args      []any
		}{"attempt-owner-pr", `INSERT INTO review_attempt_memberships VALUES(?,'producer-a','shared-attempt',1,NULL,1,2,0)`, []any{f.project}},
		struct {
			name, sql string
			args      []any
		}{"attempt-producer", `INSERT INTO review_attempt_memberships VALUES(?,'unknown-producer','shared-attempt',1,NULL,1,1,0)`, []any{f.project}},
	)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { historyPhysicalReject(t, f, test.sql, test.args...) })
	}
}

func TestHistoryPhysicalSchemaPayloadChecksAndProducerKeys(t *testing.T) {
	f := historyPhysicalFixture(t)
	for _, statement := range []string{
		`UPDATE review_pull_versions SET complete=2 WHERE project_id=?`,
		`UPDATE review_pull_versions SET run_count=-1 WHERE project_id=?`,
		`UPDATE review_pull_versions SET attempt_count=-1 WHERE project_id=?`,
		`UPDATE review_run_versions SET attributable=2 WHERE project_id=?`,
		`UPDATE review_run_versions SET finalized=2 WHERE project_id=?`,
		`UPDATE review_run_versions SET found=-1 WHERE project_id=?`,
		`UPDATE review_attempt_versions SET duration_seconds=-1 WHERE project_id=?`,
		`UPDATE review_finding_versions SET position=-1 WHERE project_id=?`,
		`UPDATE review_retired_versions SET reason='merged' WHERE project_id=?`,
		`UPDATE review_retired_versions SET number=2147483648 WHERE project_id=?`,
		// Producer B's source ID is valid in this fixture, but cannot be
		// relabeled to producer A's already retained same-revision identity.
		`UPDATE review_run_versions SET source='producer-a' WHERE project_id=? AND id='run-two'`,
		`UPDATE review_attempt_versions SET source='producer-a' WHERE project_id=? AND source='producer-b'`,
	} {
		historyPhysicalReject(t, f, statement, f.project)
	}
	for _, statement := range []string{
		`INSERT INTO review_pull_memberships VALUES(?,1,1,NULL,1,-1)`,
		`INSERT INTO review_run_memberships VALUES(?,'run-one',1,NULL,1,1,-1)`,
		`INSERT INTO review_attempt_memberships VALUES(?,'producer-a','shared-attempt',1,NULL,1,1,-1)`,
		`INSERT INTO review_retired_memberships VALUES(?,8,1,NULL,1,-1)`,
	} {
		historyPhysicalReject(t, f, statement, f.project)
	}
}

func TestHistoryPhysicalSchemaClosureOpenUniquenessAndCurrentOwnerOrder(t *testing.T) {
	f := historyPhysicalFixture(t)
	historyPhysicalMemberships(t, f)
	for _, table := range []string{"review_pull_memberships", "review_run_memberships", "review_attempt_memberships", "review_retired_memberships"} {
		t.Run(table, func(t *testing.T) {
			for _, end := range []int{0, 1, 9} {
				historyPhysicalReject(t, f, `UPDATE `+table+` SET end_revision=? WHERE project_id=?`, end, f.project)
			}
			// A second still-open interval for the same entity is disallowed even
			// when its snapshot and exact version references are otherwise valid.
			columns := map[string]string{"review_pull_memberships": "project_id,number,3,NULL,version_revision,position", "review_run_memberships": "project_id,id,3,NULL,version_revision,pull_number,run_order", "review_attempt_memberships": "project_id,source,source_attempt_id,3,NULL,version_revision,pull_number,position", "review_retired_memberships": "project_id,number,3,NULL,version_revision,position"}[table]
			historyPhysicalReject(t, f, `INSERT INTO `+table+` SELECT `+columns+` FROM `+table+` WHERE project_id=?`, f.project)
		})
	}
	for _, table := range []string{"review_current_pulls", "review_current_runs", "review_current_attempts", "review_current_retired"} {
		order := "position"
		if table == "review_current_runs" {
			order = "run_order"
		}
		historyPhysicalReject(t, f, `UPDATE `+table+` SET `+order+`=99 WHERE project_id=?`, f.project)
		historyPhysicalReject(t, f, `UPDATE `+table+` SET start_revision=2 WHERE project_id=?`, f.project)
		historyPhysicalReject(t, f, `UPDATE `+table+` SET project_id='history-other' WHERE project_id=?`, f.project)
		if table == "review_current_runs" || table == "review_current_attempts" {
			historyPhysicalReject(t, f, `UPDATE `+table+` SET pull_number=2 WHERE project_id=?`, f.project)
		}
	}
	// Position uniqueness is enforced for otherwise valid, distinct identities.
	for _, test := range []struct{ membership, current string }{
		{`INSERT INTO review_pull_memberships VALUES(?,2,1,NULL,1,0)`, `INSERT INTO review_current_pulls VALUES(?,2,1,0)`},
		{`INSERT INTO review_run_memberships VALUES(?,'run-two',1,NULL,1,1,0)`, `INSERT INTO review_current_runs VALUES(?,'run-two',1,1,0)`},
		{`INSERT INTO review_attempt_memberships VALUES(?,'producer-b','shared-attempt',1,NULL,1,1,0)`, `INSERT INTO review_current_attempts VALUES(?,'producer-b','shared-attempt',1,1,0)`},
		{`INSERT INTO review_retired_memberships VALUES(?,9,1,NULL,1,0)`, `INSERT INTO review_current_retired VALUES(?,9,1,0)`},
	} {
		historyPhysicalExec(t, f, test.membership, f.project)
		historyPhysicalReject(t, f, test.current, f.project)
	}
}

func TestHistoryPhysicalSchemaRetainedReferencesNeverCascade(t *testing.T) {
	f := historyPhysicalFixture(t)
	historyPhysicalMemberships(t, f)
	for _, statement := range []string{
		`DELETE FROM review_snapshots WHERE project_id=? AND revision=1`,
		`DELETE FROM review_pull_versions WHERE project_id=? AND number=1 AND version_revision=1`,
		`DELETE FROM review_run_versions WHERE project_id=? AND id='run-one' AND version_revision=1`,
		`DELETE FROM review_attempt_versions WHERE project_id=? AND source='producer-a' AND version_revision=1`,
		`DELETE FROM review_retired_versions WHERE project_id=? AND number=8 AND version_revision=1`,
		`DELETE FROM review_pull_memberships WHERE project_id=? AND number=1`,
		`DELETE FROM review_run_memberships WHERE project_id=? AND id='run-one'`,
		`DELETE FROM review_attempt_memberships WHERE project_id=? AND source='producer-a'`,
		`DELETE FROM review_retired_memberships WHERE project_id=? AND number=8`,
		`DELETE FROM projects WHERE id=?`,
	} {
		historyPhysicalReject(t, f, statement, f.project)
	}
	// Every new reference declares both actions explicitly: changing an old
	// payload owner must not rewrite references under an apparent valid history.
	for _, table := range []string{"review_pull_versions", "review_run_versions", "review_finding_versions", "review_attempt_versions", "review_retired_versions", "review_pull_memberships", "review_run_memberships", "review_attempt_memberships", "review_retired_memberships", "review_current_pulls", "review_current_runs", "review_current_attempts", "review_current_retired"} {
		rows, err := f.store.DB().Query(`PRAGMA foreign_key_list("` + table + `")`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, seq int
			var parent, from, to, update, del, match string
			if err := rows.Scan(&id, &seq, &parent, &from, &to, &update, &del, &match); err != nil {
				t.Fatal(err)
			}
			if update != "NO ACTION" || del != "NO ACTION" {
				t.Fatalf("retention reference cascades in %s: %s/%s", table, update, del)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestHistoryPhysicalSchemaPageIndexes(t *testing.T) {
	f := historyPhysicalFixture(t)
	historyPhysicalMemberships(t, f)
	tests := []struct {
		name, query string
		args        []any
	}{
		{"current-run", `SELECT id FROM review_current_runs WHERE project_id=? AND pull_number=1 AND run_order<100 ORDER BY run_order DESC LIMIT 21`, []any{f.project}},
		{"current-attempt", `SELECT source_attempt_id FROM review_current_attempts WHERE project_id=? AND pull_number=1 AND position<100 ORDER BY position DESC LIMIT 21`, []any{f.project}},
		{"current-pull", `SELECT number FROM review_current_pulls WHERE project_id=? AND position> -1 ORDER BY position LIMIT 21`, []any{f.project}},
		{"current-retired", `SELECT number FROM review_current_retired WHERE project_id=? AND position> -1 ORDER BY position LIMIT 21`, []any{f.project}},
		{"historical-run", `SELECT id FROM review_run_memberships WHERE project_id=? AND pull_number=1 AND run_order<100 AND start_revision<=1 AND (end_revision IS NULL OR 1<end_revision) ORDER BY run_order DESC LIMIT 21`, []any{f.project}},
		{"historical-attempt", `SELECT source_attempt_id FROM review_attempt_memberships WHERE project_id=? AND pull_number=1 AND position<100 AND start_revision<=1 AND (end_revision IS NULL OR 1<end_revision) ORDER BY position DESC LIMIT 21`, []any{f.project}},
		{"historical-pull", `SELECT number FROM review_pull_memberships WHERE project_id=? AND position> -1 AND start_revision<=1 AND (end_revision IS NULL OR 1<end_revision) ORDER BY position LIMIT 21`, []any{f.project}},
		{"historical-retired", `SELECT number FROM review_retired_memberships WHERE project_id=? AND position> -1 AND start_revision<=1 AND (end_revision IS NULL OR 1<end_revision) ORDER BY position LIMIT 21`, []any{f.project}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rows, err := f.store.DB().Query("EXPLAIN QUERY PLAN "+test.query, test.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				details = append(details, detail)
			}
			if rows.Err() != nil {
				t.Fatal(rows.Err())
			}
			plan := strings.Join(details, "\n")
			if !strings.Contains(plan, "USING INDEX") && !strings.Contains(plan, "USING COVERING INDEX") || strings.Contains(plan, "TEMP B-TREE") || strings.Contains(plan, "SCAN ") {
				t.Fatalf("page lacks indexed ordered access: %s", plan)
			}
		})
	}
}
