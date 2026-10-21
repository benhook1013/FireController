package reviews

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

type historyMigrationWire struct {
	Status int
	Header http.Header
	Body   string
}

func historyMigrationRead(t *testing.T, f *apiFixture, route string) historyMigrationWire {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews"+route, nil)
	req.Header.Set("Authorization", "Bearer "+f.readToken)
	out := httptest.NewRecorder()
	out.Header().Set("X-Request-ID", "migration-wire-proof")
	metadataFixtureReaders(f.store, f.mux).ServeHTTP(out, req)
	// Compare the preexisting wire contract; schema nine adds independently tested channel totals.
	body := regexp.MustCompile(`,"run_counts":\{[^}]*\}`).ReplaceAllString(out.Body.String(), "")
	return historyMigrationWire{out.Code, out.Header().Clone(), body}
}
func historyMigrationExec(t *testing.T, tx *sql.Tx, statement string, args ...any) {
	t.Helper()
	if _, err := tx.ExecContext(t.Context(), statement, args...); err != nil {
		t.Fatalf("migration fixture: %v\n%s", err, statement)
	}
}
func historyMigrationSnapshot(t *testing.T, tx *sql.Tx, project, id string, revision int) {
	t.Helper()
	historyMigrationExec(t, tx, `INSERT INTO review_snapshots VALUES(?,?,?,701,'transfer-publisher','2026-10-01T00:00:00Z','2026-10-01T00:01:00Z',?)`, project, id, revision, strings.Repeat("a", 64))
}
func historyMigrationPull(t *testing.T, tx *sql.Tx, project, snapshot string, revision, number, position int, producer string, run bool, routed bool) string {
	t.Helper()
	external := "shared-run"
	id := runOpaqueID(project, producer, external)
	owner := number
	count := 0
	recent := []RunSummary{}
	latest := []AttemptSummary{}
	finalized := producer != "producer-b" // Preserve old nonfinalized owner movement.
	scope := "Retained comparison"
	if !finalized {
		scope = fmt.Sprintf("old scope %d", revision)
	}
	state := "completed"
	finished := "2026-10-01T00:01:00Z"
	var duration any = int64(60)
	if revision == 1 {
		state = "active"
		finished = ""
		duration = nil
	}
	found, accepted, routes := int64(0), int64(0), int64(0)
	if routed {
		found, routes = 1, 1
	}
	if run {
		count = 1
		recent = append(recent, RunSummary{Source: producer, ID: id, PullNumber: int64(number), SourceRunID: external, Channel: "cli", HeadSHA: strings.Repeat("c", 40), Reviewer: "Retained reviewer", Scope: scope, Outcome: "completed", Attributable: true, Complete: true, StartedAt: "2026-10-01T00:00:00Z", FinishedAt: "2026-10-01T00:01:00Z", Found: found, Accepted: accepted, Routed: routes, Finalized: finalized, CoverageLimits: []string{}})
		var ptr *int64
		if duration != nil {
			d := int64(60)
			ptr = &d
		}
		latest = append(latest, AttemptSummary{SourceAttemptID: "shared-attempt", Channel: "cli", HeadSHA: strings.Repeat("c", 40), State: state, StartedAt: "2026-10-01T00:00:00Z", FinishedAt: finished, DurationSeconds: ptr, SourceRunID: external})
	}
	historyMigrationExec(t, tx, `INSERT INTO review_pulls VALUES(?,?,?,?,?,'merged',?,'main',?,'unknown','unknown',1,?,?,?,?)`, project, snapshot, position, number, fmt.Sprintf("Retained PR%d at%d", number, revision), strings.Repeat("c", 40), strings.Repeat("d", 40), count, count, mustJSON(recent), mustJSON(latest))
	if !run {
		return ""
	}
	historyMigrationExec(t, tx, `INSERT INTO review_runs(project_id,snapshot_id,pull_number,run_order,id,source,source_run_id,channel,head_sha,reviewer,scope,outcome,attributable,complete,started_at,finished_at,found,accepted,routed,finalized,fixed,coverage_limits_json,snapshot_revision) VALUES(?,?,?,0,?,?,'shared-run','cli',?,'Retained reviewer',?,'completed',1,1,'2026-10-01T00:00:00Z','2026-10-01T00:01:00Z',?,?,?,?,0,'[]',?)`, project, snapshot, owner, id, producer, strings.Repeat("c", 40), scope, found, accepted, routes, finalized, revision)
	historyMigrationExec(t, tx, `INSERT INTO review_attempts VALUES(?,?,?,0,?,'shared-attempt','cli',?,?,'2026-10-01T00:00:00Z',?,?,'shared-run','','')`, project, snapshot, owner, producer, strings.Repeat("c", 40), state, finished, duration)
	if routed {
		route := nativeRouteFixture(0)
		route.TargetPullNumber = 8
		if revision == 9 {
			route = importedFixedRoute()
			route.TargetPullNumber = 8
		}
		historyMigrationExec(t, tx, `INSERT INTO review_findings VALUES(?,?,?,?,0,'retained-finding','Immutable title','Immutable body <literal>','medium','routed','Original decision','original-actor','2026-10-01T00:01:00Z','','','',?)`, project, snapshot, owner, id, mustJSON(route))
	}
	return id
}

func historyMigrationLegacyFixture(t *testing.T) (*apiFixture, []string, string, string) {
	t.Helper()
	f := newLegacyAPIFixture(t)
	tx := historyWriterTx(t, f)
	snapshots := []string{"legacy-one", "legacy-four", "legacy-nine"}
	for i, revision := range []int{1, 4, 9} {
		historyMigrationSnapshot(t, tx, f.project, snapshots[i], revision)
	}
	idA := historyMigrationPull(t, tx, f.project, snapshots[0], 1, 7, 0, "producer-a", true, true)
	idB := historyMigrationPull(t, tx, f.project, snapshots[0], 1, 8, 1, "producer-b", true, false)
	historyMigrationPull(t, tx, f.project, snapshots[1], 4, 8, 0, "producer-b", true, false)
	historyMigrationPull(t, tx, f.project, snapshots[2], 9, 7, 1, "producer-a", true, true)
	historyMigrationPull(t, tx, f.project, snapshots[2], 9, 8, 0, "producer-b", true, false)
	// Genuine v3 storage permitted nonfinalized historical ownership movement.
	// Preserve that old fact; new publication will enforce its stricter contract.
	historyMigrationExec(t, tx, `UPDATE review_runs SET pull_number=7,run_order=1 WHERE project_id=? AND snapshot_id=? AND id=?`, f.project, snapshots[0], idB)
	historyMigrationExec(t, tx, `UPDATE review_attempts SET pull_number=7,position=1 WHERE project_id=? AND snapshot_id=? AND source='producer-b'`, f.project, snapshots[0])
	historyMigrationExec(t, tx, `UPDATE review_pulls SET run_count=2,attempt_count=2 WHERE project_id=? AND snapshot_id=? AND number=7`, f.project, snapshots[0])
	historyMigrationExec(t, tx, `UPDATE review_pulls SET run_count=0,attempt_count=0,recent_runs_json='[]',latest_attempts_json='[]' WHERE project_id=? AND snapshot_id=? AND number=8`, f.project, snapshots[0])
	for _, snapshot := range []string{snapshots[0], snapshots[2]} {
		reason := "defunct"
		if snapshot == snapshots[2] {
			reason = "superseded"
		}
		historyMigrationExec(t, tx, `INSERT INTO review_retired_pulls VALUES(?,?,0,99,?)`, f.project, snapshot, reason)
	}
	historyMigrationExec(t, tx, `INSERT INTO review_workspace VALUES(?,9,?)`, f.project, snapshots[2])
	// Interleaved revisions catch an accidental LEAD without project partition.
	historyMigrationExec(t, tx, `INSERT INTO projects VALUES('migration-other','Other',702,'owner/other')`)
	for _, revision := range []int{2, 6} {
		historyMigrationSnapshot(t, tx, "migration-other", fmt.Sprintf("other-%d", revision), revision)
	}
	historyMigrationPull(t, tx, "migration-other", "other-2", 2, 500, 0, "producer-a", true, false)
	historyMigrationExec(t, tx, `INSERT INTO review_workspace VALUES('migration-other',6,'other-6')`)
	historyMigrationExec(t, tx, `INSERT INTO receipts VALUES(?,'migration-principal',?,'reviews.observe',?,'original-key','retained-fingerprint',?)`, f.store.InstanceID(), f.project, f.project, "{\"id\":\"legacy-nine\",\"revision\":9,\"outcome\":\"recorded\"}\n")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return f, snapshots, idA, idB
}

// SQL and data fingerprints use actual persisted rows, not a reconstructed
// response. Comparing them on failure catches partially renamed/dropped tables,
// metadata advancement, data cascades and leaked temporary assertion objects.
func historyMigrationDatabaseDigest(t *testing.T, f *apiFixture) [32]byte {
	t.Helper()
	h := sha256.New()
	rows, err := f.store.DB().QueryContext(t.Context(), `SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var kind, name, table string
		var ddl sql.NullString
		if err := rows.Scan(&kind, &name, &table, &ddl); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		encoded, _ := json.Marshal([]any{kind, name, table, ddl})
		h.Write(encoded)
		if kind == "table" && !strings.HasPrefix(name, "sqlite_") {
			tables = append(tables, name)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(tables)
	for _, table := range tables {
		rows, err := f.store.DB().QueryContext(t.Context(), `SELECT * FROM "`+table+`" ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		h.Write([]byte(table))
		for rows.Next() {
			values, targets := make([]any, len(columns)), make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err := rows.Scan(targets...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			encoded, err := json.Marshal(values)
			if err != nil {
				rows.Close()
				t.Fatal(err)
			}
			h.Write(encoded)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}
func historyMigrationApply(t *testing.T, f *apiFixture) {
	t.Helper()
	groups := append(reviewSchemaMigrations(), historyMigrationSQL())
	if err := f.store.EnsureSchema(t.Context(), "reviews", groups...); err != nil {
		t.Fatal("manual version-four migration", err)
	}
}
func historyMigrationNoTemporaryObjects(t *testing.T, f *apiFixture) {
	t.Helper()
	var count int
	if err := f.store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_temp_schema WHERE name='review_history_migration_assert'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("migration assertion leaked", count, err)
	}
}

func TestHistoryMigrationPreservesRetainedHTTPBytesAndReferences(t *testing.T) {
	f, snapshots, idA, idB := historyMigrationLegacyFixture(t)
	routes := []string{"/queue?limit=1"}
	for _, snapshot := range snapshots {
		for _, number := range []int{7, 8} {
			routes = append(routes, fmt.Sprintf("/pulls/%d?snapshot=%s", number, snapshot), fmt.Sprintf("/pulls/%d/runs?snapshot=%s&limit=1", number, snapshot), fmt.Sprintf("/pulls/%d/attempts?snapshot=%s&limit=1", number, snapshot))
		}
		for _, id := range []string{idA, idB} {
			routes = append(routes, "/runs/"+id+"?snapshot="+snapshot)
		}
	}
	expected := map[string]historyMigrationWire{}
	for _, route := range routes {
		wire := historyMigrationRead(t, f, route)
		expected[route] = wire
		if wire.Status == 200 {
			var page struct {
				Cursor string `json:"cursor"`
			}
			if err := json.Unmarshal([]byte(wire.Body), &page); err != nil {
				t.Fatal(err)
			}
			if page.Cursor != "" {
				continuation := strings.Split(route, "?")[0] + "?limit=1&cursor=" + url.QueryEscape(page.Cursor)
				expected[continuation] = historyMigrationRead(t, f, continuation)
			}
		}
	}
	var receiptBefore string
	if err := f.store.DB().QueryRowContext(t.Context(), `SELECT response_json FROM receipts WHERE key='original-key'`).Scan(&receiptBefore); err != nil {
		t.Fatal(err)
	}
	var pagesBefore, pageSize int
	if err := f.store.DB().QueryRowContext(t.Context(), `PRAGMA page_count`).Scan(&pagesBefore); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRowContext(t.Context(), `PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	historyMigrationApply(t, f)
	elapsed := time.Since(started)
	for route, want := range expected {
		if got := historyMigrationRead(t, f, route); !reflect.DeepEqual(got, want) {
			t.Fatalf("migration changed status/headers/body at %s\nbefore=%+v\nafter=%+v", route, want, got)
		}
	}
	var receiptAfter string
	if err := f.store.DB().QueryRowContext(t.Context(), `SELECT response_json FROM receipts WHERE key='original-key'`).Scan(&receiptAfter); err != nil || receiptAfter != receiptBefore {
		t.Fatal("receipt bytes changed", err)
	}
	if err := ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	var end int
	if err := f.store.DB().QueryRowContext(t.Context(), `SELECT end_revision FROM review_run_memberships WHERE project_id=? AND id=? AND start_revision=1`, f.project, idA).Scan(&end); err != nil || end != 4 {
		t.Fatal("omission gap closed at next identity instead of project snapshot", end, err)
	}
	var count int
	if err := f.store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM review_runs WHERE project_id=? AND snapshot_id=? AND id=?`, f.project, snapshots[1], idA).Scan(&count); err != nil || count != 0 {
		t.Fatal("omitted run resurrected", count, err)
	}
	for _, test := range []struct {
		snapshot string
		pull     int
	}{{snapshots[0], 7}, {snapshots[1], 8}, {snapshots[2], 8}} {
		var owner int
		if err := f.store.DB().QueryRowContext(t.Context(), `SELECT pull_number FROM review_runs WHERE project_id=? AND snapshot_id=? AND id=?`, f.project, test.snapshot, idB).Scan(&owner); err != nil || owner != test.pull {
			t.Fatal("old nonfinalized owner movement changed", owner, err)
		}
	}
	beforeRetry := historyMigrationDatabaseDigest(t, f)
	historyMigrationApply(t, f)
	if beforeRetry != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("schema retry changed data/DDL")
	}
	historyMigrationNoTemporaryObjects(t, f)
	// Reopen actual retained storage and reapply the complete selected list. No
	// old mutation handler is called against the migrated read-only views.
	var sequence int
	var databaseName, databasePath string
	if err := f.store.DB().QueryRowContext(t.Context(), `PRAGMA database_list`).Scan(&sequence, &databaseName, &databasePath); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(t.Context(), filepath.Dir(databasePath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	f.store = reopened
	mux := http.NewServeMux()
	Register(mux, reopened)
	f.mux = mux
	historyMigrationApply(t, f)
	for route, want := range expected {
		if got := historyMigrationRead(t, f, route); !reflect.DeepEqual(got, want) {
			t.Fatal("restart changed retained wire bytes", route)
		}
	}
	var pagesAfter int
	if err := f.store.DB().QueryRowContext(t.Context(), `PRAGMA page_count`).Scan(&pagesAfter); err != nil {
		t.Fatal(err)
	}
	t.Logf("actual migration: snapshots=5 runs=6 elapsed=%s logical_pages_before=%d after=%d page_size=%d (small fixture, no scale guarantee)", elapsed, pagesBefore, pagesAfter, pageSize)
}

func historyMigrationDowngradeFixture(t *testing.T, f *apiFixture, version int) {
	t.Helper()
	tx := historyWriterTx(t, f)
	historyMigrationExec(t, tx, `ALTER TABLE review_findings DROP COLUMN route_json`)
	if version == 1 {
		for _, statement := range []string{`DROP TRIGGER review_runs_snapshot_revision_insert`, `DROP TRIGGER review_runs_snapshot_revision_update`, `DROP INDEX review_runs_project_source_id`, `ALTER TABLE review_runs DROP COLUMN snapshot_revision`, `CREATE INDEX review_runs_project_source_id ON review_runs(project_id,source,source_run_id,finalized)`} {
			historyMigrationExec(t, tx, statement)
		}
	}
	historyMigrationExec(t, tx, `UPDATE metadata SET value=? WHERE key='schema:reviews'`, fmt.Sprint(version))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
func TestHistoryMigrationFromSupportedOlderGroups(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("version-%d", version), func(t *testing.T) {
			f, snapshots, idA, _ := historyMigrationLegacyFixture(t)
			// These older source formats never had receiver-route JSON. Establish the
			// same-format reference bytes before reconstructing their exact old columns.
			if _, err := f.store.DB().ExecContext(t.Context(), `UPDATE review_findings SET route_json='null'`); err != nil {
				t.Fatal(err)
			}
			routes := []string{"/queue?limit=1", "/pulls/7?snapshot=" + snapshots[0], "/runs/" + idA + "?snapshot=" + snapshots[0], "/runs/" + idA + "?snapshot=" + snapshots[2]}
			want := map[string]historyMigrationWire{}
			for _, route := range routes {
				want[route] = historyMigrationRead(t, f, route)
			}
			historyMigrationDowngradeFixture(t, f, version)
			historyMigrationApply(t, f)
			for route, expected := range want {
				if got := historyMigrationRead(t, f, route); !reflect.DeepEqual(got, expected) {
					t.Fatal("older supported source changed retained wire facts", version, route, got, expected)
				}
			}
			historyMigrationNoTemporaryObjects(t, f)
		})
	}
}

func TestHistoryMigrationInvalidSourcesRefuseAtomically(t *testing.T) {
	tests := []struct {
		name   string
		damage func(*testing.T, *apiFixture)
	}{
		{"owning-revision", func(t *testing.T, f *apiFixture) {
			for _, s := range []string{`DROP TRIGGER review_runs_snapshot_revision_update`, `UPDATE review_runs SET snapshot_revision=0 WHERE snapshot_id='legacy-one'`} {
				if _, err := f.store.DB().ExecContext(t.Context(), s); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"workspace-revision", func(t *testing.T, f *apiFixture) {
			if _, err := f.store.DB().ExecContext(t.Context(), `UPDATE review_workspace SET revision=8 WHERE project_id=?`, f.project); err != nil {
				t.Fatal(err)
			}
		}},
		{"orphan-project-history", func(t *testing.T, f *apiFixture) {
			if _, err := f.store.DB().ExecContext(t.Context(), `DELETE FROM review_workspace WHERE project_id=?`, f.project); err != nil {
				t.Fatal(err)
			}
		}},
		{"later-snapshot", func(t *testing.T, f *apiFixture) {
			if _, err := f.store.DB().ExecContext(t.Context(), `UPDATE review_workspace SET snapshot_id='legacy-four',revision=4 WHERE project_id=?`, f.project); err != nil {
				t.Fatal(err)
			}
		}},
		{"foreign-key-source", func(t *testing.T, f *apiFixture) {
			conn, err := f.store.DB().Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF`); err != nil {
				t.Fatal(err)
			}
			if _, err = conn.ExecContext(t.Context(), `UPDATE review_attempts SET pull_number=999 WHERE project_id=? AND snapshot_id='legacy-one'`, f.project); err != nil {
				t.Fatal(err)
			}
			if _, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=ON`); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f, _, _, _ := historyMigrationLegacyFixture(t)
			test.damage(t, f)
			before := historyMigrationDatabaseDigest(t, f)
			groups := append(reviewSchemaMigrations(), historyMigrationSQL())
			if err := f.store.EnsureSchema(t.Context(), "reviews", groups...); err == nil {
				t.Fatal("invalid source migrated")
			}
			if before != historyMigrationDatabaseDigest(t, f) {
				t.Fatal("refused migration changed old schema, metadata or rows")
			}
			var version string
			if err := f.store.DB().QueryRowContext(t.Context(), `SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&version); err != nil || version != "3" {
				t.Fatal("refusal advanced source version", version, err)
			}
		})
	}
}

func TestHistoryMigrationPartialFaultRollsBackDDLAndRows(t *testing.T) {
	sqlGroup := historyMigrationSQL()
	cuts := []int{}
	for i, statement := range sqlGroup {
		if strings.HasPrefix(statement, "ALTER TABLE review_pulls ") || strings.HasPrefix(statement, "INSERT INTO review_finding_versions") || statement == "DROP TABLE legacy_review_pulls;" || strings.HasPrefix(statement, "CREATE VIEW review_attempts ") {
			cuts = append(cuts, i+1)
		}
	}
	if len(cuts) != 4 {
		t.Fatal("fault checkpoints missing")
	}
	for _, cut := range cuts {
		t.Run(fmt.Sprintf("statement-%d", cut), func(t *testing.T) {
			f, _, _, _ := historyMigrationLegacyFixture(t)
			before := historyMigrationDatabaseDigest(t, f)
			fault := append([]string{}, sqlGroup[:cut]...)
			fault = append(fault, "SELECT definitely_missing_history_migration_function();")
			fault = append(fault, sqlGroup[cut:]...)
			groups := append(reviewSchemaMigrations(), fault)
			if err := f.store.EnsureSchema(t.Context(), "reviews", groups...); err == nil {
				t.Fatal("injected partial migration succeeded")
			}
			if before != historyMigrationDatabaseDigest(t, f) {
				t.Fatal("partial migration failed to restore exact v3 tables/metadata/rows")
			}
			// Core reuses its exclusive connection. A retry also proves rollback removed
			// the temporary assertion there, not just from an unrelated read connection.
			historyMigrationApply(t, f)
			historyMigrationNoTemporaryObjects(t, f)
		})
	}
}

func TestHistoryMigrationRetainedBodyFootprintObservation(t *testing.T) {
	const snapshots, runs, findings = 64, 8, 4
	f := newLegacyAPIFixture(t)
	tx := historyWriterTx(t, f)
	detail := strings.Repeat("retained body <literal> ", 24)
	for revision := 1; revision <= snapshots; revision++ {
		snapshot := fmt.Sprintf("footprint-%d", revision)
		historyMigrationSnapshot(t, tx, f.project, snapshot, revision)
		historyMigrationExec(t, tx, `INSERT INTO review_pulls VALUES(?,?,0,7,'Retained','','','','','','',1,?,0,'[]','[]')`, f.project, snapshot, runs)
		for run := 0; run < runs; run++ {
			external := fmt.Sprintf("retained-%d", run)
			id := runOpaqueID(f.project, "producer", external)
			historyMigrationExec(t, tx, `INSERT INTO review_runs VALUES(?,?,7,?,?, 'producer',?,'cli','','Retained reviewer','Retained scope','completed',1,1,'','',?,0,0,0,0,'[]',?)`, f.project, snapshot, run, id, external, findings, revision)
			for finding := 0; finding < findings; finding++ {
				historyMigrationExec(t, tx, `INSERT INTO review_findings VALUES(?,?,7,?,?,?,'Title',?,'medium','unadjudicated','','','','','','','null')`, f.project, snapshot, id, finding, fmt.Sprintf("finding-%d", finding), detail)
			}
		}
	}
	historyMigrationExec(t, tx, `INSERT INTO review_workspace VALUES(?,?,?)`, f.project, snapshots, fmt.Sprintf("footprint-%d", snapshots))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var beforePages, afterPages, freeBefore, freeAfter, pageSize, bytesBefore, bytesAfter int
	for _, probe := range []struct {
		sql string
		dst *int
	}{{`PRAGMA page_count`, &beforePages}, {`PRAGMA freelist_count`, &freeBefore}, {`PRAGMA page_size`, &pageSize}, {`SELECT sum(length(CAST(detail AS BLOB))) FROM review_findings`, &bytesBefore}} {
		if err := f.store.DB().QueryRowContext(t.Context(), probe.sql).Scan(probe.dst); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	historyMigrationApply(t, f)
	elapsed := time.Since(started)
	if err := f.store.DB().QueryRowContext(t.Context(), `PRAGMA page_count`).Scan(&afterPages); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRowContext(t.Context(), `SELECT sum(length(CAST(detail AS BLOB))) FROM review_finding_versions`).Scan(&bytesAfter); err != nil || bytesAfter != bytesBefore || bytesAfter != snapshots*runs*findings*len(detail) {
		t.Fatal("retained bodies discarded/duplicated", bytesBefore, bytesAfter, err)
	}
	var versions int
	if err := f.store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM review_run_versions`).Scan(&versions); err != nil || versions != snapshots*runs {
		t.Fatal("not one exact version per old run row", versions, err)
	}
	if err := f.store.DB().QueryRowContext(t.Context(), `PRAGMA freelist_count`).Scan(&freeAfter); err != nil {
		t.Fatal(err)
	}
	t.Logf("actual bounded retained-body migration: snapshots=%d runs=%d findings=%d detail_bytes=%d elapsed=%s pages_before=%d after=%d page_size=%d freelist_before=%d after=%d; no latency threshold or large-installation guarantee", snapshots, snapshots*runs, snapshots*runs*findings, bytesAfter, elapsed, beforePages, afterPages, pageSize, freeBefore, freeAfter)
}

func TestHistoryMigrationPostBackfillLogicalCheckRollsBack(t *testing.T) {
	f, _, _, _ := historyMigrationLegacyFixture(t)
	before := historyMigrationDatabaseDigest(t, f)
	migration := historyMigrationSQL()
	cut := 0
	for i, statement := range migration {
		if strings.HasPrefix(statement, "CREATE VIEW ") {
			cut = i + 1
		}
	}
	if cut == 0 {
		t.Fatal("canonical views missing")
	}
	// This alteration obeys physical foreign keys. The canonical logical graph
	// assertion must reject an otherwise valid payload omitted from current refs.
	corrupted := append([]string{}, migration[:cut]...)
	corrupted = append(corrupted, `DELETE FROM review_current_runs WHERE project_id='project-review-test'`)
	corrupted = append(corrupted, migration[cut:]...)
	groups := append(reviewSchemaMigrations(), corrupted)
	if err := f.store.EnsureSchema(t.Context(), "reviews", groups...); err == nil {
		t.Fatal("incomplete current graph passed migration checks")
	}
	if before != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("logical check failure changed v3 schema/data/metadata")
	}
	historyMigrationApply(t, f)
}
