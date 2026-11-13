package jobs

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func oldProgressStore(t *testing.T) *core.Store {
	t.Helper()
	s, err := core.Open(t.Context(), filepath.Join(t.TempDir(), "old-jobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.EnsureSchema(t.Context(), "jobs", schemaMigrations()[:7]...); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec(`INSERT INTO projects VALUES('project_old','Old',991,'synthetic/old');INSERT INTO jobs VALUES('project_old','job_old','Original title','Original brief',NULL,'active',4,'2026-01-01T00:00:00Z','2026-01-01T00:00:10Z');INSERT INTO jobs VALUES('project_old','job_empty','Empty','No old progress',NULL,'parked',2,'2026-01-01T00:00:00Z','2026-01-01T00:00:10Z');INSERT INTO job_history VALUES('project_old','job_old',1,'original-actor','{"original":"immutable legacy audit"}','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	return s
}
func seedLegacyCheckpoint(t *testing.T, s *core.Store, id, summary, next, blocker, at string) {
	t.Helper()
	if _, err := s.DB().Exec(`INSERT INTO job_checkpoints(id,project_id,job_id,job_revision,actor_id,worker_id,done,next,blocker,evidence_json,created_at) VALUES(?,'project_old','job_old',2,'checkpoint-actor','',?,?,?,'[]',?)`, id, summary, next, blocker, at); err != nil {
		t.Fatal(err)
	}
}
func seedLegacyUpdate(t *testing.T, s *core.Store, id, body, refs, at string) {
	t.Helper()
	if _, err := s.DB().Exec(`INSERT INTO job_updates(id,project_id,job_id,job_revision,actor_id,worker_id,body,evidence_json,created_at) VALUES(?,'project_old','job_old',3,'update-actor','',?,?,?)`, id, body, refs, at); err != nil {
		t.Fatal(err)
	}
}
func progressSourceRows(t *testing.T, s *core.Store) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range []string{"jobs", "job_history", "job_checkpoints", "job_updates"} {
		rows, err := s.DB().Query(`SELECT * FROM ` + table + ` ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		var records [][]any
		for rows.Next() {
			v := make([]any, len(cols))
			dest := make([]any, len(cols))
			for i := range v {
				dest[i] = &v[i]
			}
			if err = rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			records = append(records, v)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		raw, _ := json.Marshal(records)
		out[table] = string(raw)
	}
	return out
}
func validateProgressStore(t *testing.T, s *core.Store) {
	t.Helper()
	tx, err := s.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := ValidateProgressSnapshot(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
}
func TestUnifiedProgressMigrationReplaysActualTimesAndPreservesArchives(t *testing.T) {
	s := oldProgressStore(t)
	seedLegacyCheckpoint(t, s, "checkpoint-new", "checkpoint summary", "next unchanged", "blocker unchanged", "2026-01-01T00:00:02Z")
	seedLegacyCheckpoint(t, s, "checkpoint-old", "earlier", "earlier next", "earlier blocker", "2026-01-01T01:00:01+01:00")
	seedLegacyUpdate(t, s, "update-final", "new narrative", "[\"original-ref\"]", "2026-01-01T00:00:03Z")
	before := progressSourceRows(t, s)
	if err := EnsureSchema(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, progressSourceRows(t, s)) {
		t.Fatal("migration rewrote source archives/job identity/history")
	}
	got, rev, err := readProgress(t.Context(), s.DB(), "project_old", "job_old", 4)
	if err != nil || got.Summary != "new narrative" || got.Next != "next unchanged" || got.Blocker != "blocker unchanged" || !reflect.DeepEqual(got.Evidence, []string{"original-ref"}) || rev != 3 {
		t.Fatal("migration lost latest composite progress", got, rev, err)
	}
	empty, rev, err := readProgress(t.Context(), s.DB(), "project_old", "job_empty", 2)
	if err != nil || !reflect.DeepEqual(empty, emptyProgress()) || rev != 2 {
		t.Fatal("empty current migration fabricated progress", err)
	}
	var count, emptyCount int
	if err := s.DB().QueryRow(`SELECT count(*) FROM job_progress_history`).Scan(&count); err != nil || count != 3 {
		t.Fatal(count, err)
	}
	if err := s.DB().QueryRow(`SELECT count(*) FROM job_progress_history WHERE job_id='job_empty'`).Scan(&emptyCount); err != nil || emptyCount != 0 {
		t.Fatal("fabricated empty history", err)
	}
	var firstSummary string
	if err := s.DB().QueryRow(`SELECT summary FROM job_progress_history ORDER BY sequence LIMIT 1`).Scan(&firstSummary); err != nil || firstSummary != "earlier" {
		t.Fatal("timezone/input sequence selected wrong chronological source", firstSummary, err)
	}
	validateProgressStore(t, s)
	if err := EnsureSchema(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	var again int
	s.DB().QueryRow(`SELECT count(*) FROM job_progress_history`).Scan(&again)
	if again != count {
		t.Fatal("restart duplicated conversion")
	}
}
func TestUnifiedProgressMigrationOverflowRetainsWholeNewestRecord(t *testing.T) {
	s := oldProgressStore(t)
	next, blocker := strings.Repeat("n", 8192), strings.Repeat("b", 4096)
	seedLegacyCheckpoint(t, s, "checkpoint-large", "old summary", next, blocker, "2026-01-01T00:00:01Z")
	body := strings.Repeat("u", 8192)
	refs := []string{strings.Repeat("r", 1024)}
	raw, _ := json.Marshal(refs)
	seedLegacyUpdate(t, s, "update-large", body, string(raw), "2026-01-01T00:00:02Z")
	before := progressSourceRows(t, s)
	if err := EnsureSchema(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	got, _, err := readProgress(t.Context(), s.DB(), "project_old", "job_old", 4)
	if err != nil || got.Summary != body || got.Next != "" || got.Blocker != "" || !reflect.DeepEqual(got.Evidence, refs) {
		t.Fatal("overflow shortened or combined unbounded newest record", err)
	}
	var priorNext, priorBlocker string
	if err := s.DB().QueryRow(`SELECT next,blocker FROM job_progress_history ORDER BY sequence LIMIT 1`).Scan(&priorNext, &priorBlocker); err != nil || priorNext != next || priorBlocker != blocker {
		t.Fatal("fallback lost earlier full canonical progress", err)
	}
	if !reflect.DeepEqual(before, progressSourceRows(t, s)) {
		t.Fatal("overflow conversion rewrote original archives")
	}
	validateProgressStore(t, s)
}
func TestUnifiedProgressMigrationMalformedSourceRollsBackSchemaAndRetries(t *testing.T) {
	s := oldProgressStore(t)
	seedLegacyUpdate(t, s, "bad-update", "valid old body", "null", "2026-01-01T00:00:02Z")
	before := progressSourceRows(t, s)
	if err := EnsureSchema(t.Context(), s); err == nil {
		t.Fatal("malformed original accepted")
	}
	var version string
	s.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:jobs'`).Scan(&version)
	var tables int
	s.DB().QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name IN ('job_progress','job_progress_history')`).Scan(&tables)
	if version != "7" || tables != 0 || !reflect.DeepEqual(before, progressSourceRows(t, s)) {
		t.Fatal("failed migration partially upgraded or rewrote source", version, tables)
	}
	if _, err := s.DB().Exec(`UPDATE job_updates SET evidence_json='[]'`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(t.Context(), s); err != nil {
		t.Fatal("retry after retained source repair failed", err)
	}
	validateProgressStore(t, s)
}

func TestUnifiedProgressMigrationPagesLegacyArchiveWithoutDroppingEntries(t *testing.T) {
	s := oldProgressStore(t)
	for i := 0; i < 65; i++ {
		seedLegacyCheckpoint(t, s, fmt.Sprintf("checkpoint-%03d", i), fmt.Sprint("state-", i), "next", "", "2026-01-01T00:00:01Z")
	}
	if err := EnsureSchema(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB().QueryRow(`SELECT count(*) FROM job_progress_history`).Scan(&count); err != nil || count != 65 {
		t.Fatal("conversion lost archive page", count, err)
	}
	current, _, err := readProgress(t.Context(), s.DB(), "project_old", "job_old", 4)
	if err != nil || current.Summary != "state-64" {
		t.Fatal("deterministic equal-time source ordering lost", current, err)
	}
	validateProgressStore(t, s)
}

func TestUnifiedProgressSnapshotRejectsMalformedJobKeyBeforePaging(t *testing.T) {
	s := oldProgressStore(t)
	if err := EnsureSchema(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO projects VALUES('','Malformed',992,'synthetic/malformed');INSERT INTO jobs VALUES('','','Title','Brief',NULL,'active',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z');INSERT INTO job_progress VALUES('','',1,'','','','[]')`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := ValidateProgressSnapshot(t.Context(), tx); err == nil {
		t.Fatal("malformed empty job key was skipped by maintenance paging")
	}
}
