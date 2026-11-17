package legacy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func snapshotFixture(t *testing.T, seed func(*sql.DB)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "selected snapshot?#.sqlite")
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("BEGIN;" + fixtureSchema); err != nil {
		db.Close()
		t.Fatal(err)
	}
	for _, q := range []string{`INSERT INTO controller_metadata VALUES(1,3,11)`, `INSERT INTO review_state VALUES(1,'{"schema_version":3}')`, `INSERT INTO job_metadata VALUES(1,2,1,3)`, `INSERT INTO inbox_metadata VALUES(1,1)`, `INSERT INTO map_metadata VALUES(1,1,1,3)`, `INSERT INTO review_records_metadata VALUES(1,9,1,3,11)`} {
		execFixture(t, db, q)
	}
	if seed != nil {
		seed(db)
	}
	execFixture(t, db, "COMMIT")
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func execFixture(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}
func inspectError(t *testing.T, err error, code string, private ...string) {
	t.Helper()
	var got *Error
	if !errors.As(err, &got) || got.Code != code || got.Message == "" {
		t.Fatalf("wanted safe %s diagnostic", code)
	}
	body, e := json.Marshal(got)
	if e != nil {
		t.Fatal(e)
	}
	for _, value := range private {
		if value != "" && strings.Contains(string(body)+err.Error(), value) {
			t.Fatal("error exposed private source data")
		}
	}
}
func sourceBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func checkConserved(t *testing.T, path string, before []byte) {
	t.Helper()
	if string(sourceBytes(t, path)) != string(before) {
		t.Fatal("snapshot bytes changed")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("snapshot inspection created a transaction file")
		}
	}
}
func seedJob(t *testing.T, db *sql.DB) {
	execFixture(t, db, `INSERT INTO jobs VALUES('private-job-id','private-name','private-worker',NULL,'private-title','active',1,1,1,'private-summary','','','[]',NULL,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
}
func seedMessage(t *testing.T, db *sql.DB, id string, reply, seen, ack any) {
	execFixture(t, db, `INSERT INTO inbox_messages VALUES(?,'private-recipient','private-message-body',NULL,NULL,NULL,?,'2026-01-01T00:00:00Z',?,?)`, id, reply, seen, ack)
}
func TestInspectSnapshotCountsBytesBlockersAndPrivacy(t *testing.T) {
	path := snapshotFixture(t, func(db *sql.DB) {
		seedJob(t, db)
		for i, brief := range []string{strings.Repeat("a", 32768), strings.Repeat("界", 12000), strings.Repeat("b", 40000)} {
			execFixture(t, db, `INSERT INTO job_briefs VALUES('private-job-id',?,'2026-01-01T00:00:00Z',?)`, i+1, brief)
		}
		for i, kind := range []string{"instruction", "reminder", "future-kind"} {
			execFixture(t, db, `INSERT INTO job_notes VALUES(?, 'private-note-body','private-worker',NULL,NULL,?,'pending','',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`, string(rune('a'+i)), kind)
		}
		for i, body := range []string{`{"kind":"instruction"}`, `{"kind":"source"}`, `{"kind":"source","kind":"instruction"}`, `{"kind":null}`} {
			execFixture(t, db, `INSERT INTO job_note_revisions VALUES('a',?,'2026-01-01T00:00:00Z',?)`, i+1, body)
		}
		seedMessage(t, db, "private-root-id", nil, "2026-01-01T00:01:00Z", "2026-01-01T00:02:00Z")
		seedMessage(t, db, "private-reply-id", "private-root-id", nil, nil)
		execFixture(t, db, `INSERT INTO map_workstreams VALUES('private-map-id',1,'private-state','private-now','private-milestone','[]','2026-01-01T00:00:00Z')`)
		execFixture(t, db, `INSERT INTO map_return_points VALUES('private-return-id',1,'private-state','2026-01-01T00:00:00Z')`)
		execFixture(t, db, `INSERT INTO review_runs VALUES('private-run-id',7,'cli',NULL,'private-reviewer','broad','[]','{}','incomplete',0,'2026-01-01T00:00:00Z',NULL,0,0,0,0,NULL)`)
		execFixture(t, db, `INSERT INTO review_attempts VALUES('private-attempt-id',7,'cli',NULL,'started','2026-01-01T00:00:00Z',NULL,NULL,NULL,NULL,NULL,NULL,NULL,'private-diagnostic','{}')`)
	})
	before := sourceBytes(t, path)
	got, err := InspectSnapshot(t.Context(), path, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	checkConserved(t, path, before)
	sum := sha256.Sum256(before)
	if got.Kind != "legacy_snapshot_inspection" || got.SHA256 != hex.EncodeToString(sum[:]) || got.Bytes != int64(len(before)) || len(got.Counts) != 37 || got.Versions != (SchemaVersions{1, 3, 11, 2, 1, 1, 9}) {
		t.Fatal("inspection metadata/count inventory incorrect")
	}
	for table, want := range map[string]int64{"jobs": 1, "job_briefs": 3, "job_notes": 3, "job_note_revisions": 4, "inbox_messages": 2, "map_workstreams": 1, "map_return_points": 1, "review_runs": 1, "review_attempts": 1} {
		if got.Counts[table] != want {
			t.Fatal("wrong source count", table)
		}
	}
	if got.Briefs != (BriefStats{3, 40000, 32768, 2, 0}) || got.Notes != (NoteStats{1, 2, 1, 1, 2}) {
		t.Fatal("byte/unsupported-history blockers lost", got.Briefs, got.Notes)
	}
	if !got.Messages.HandlingTimestampsAvailable || got.Messages.Replies != 1 || got.Messages.Seen != 1 || got.Messages.Acknowledged != 1 || got.Messages.OrphanReplies != 0 || got.Messages.CyclicReplyMessages != 0 {
		t.Fatal("message handling facts lost")
	}
	data, e := json.Marshal(got)
	if e != nil {
		t.Fatal(e)
	}
	for _, private := range []string{"private-", path, strings.Repeat("界", 10), "future-kind"} {
		if strings.Contains(string(data), private) {
			t.Fatal("ordinary inspection exposed source content")
		}
	}
}
func TestInspectSnapshotReplyAndHandlingIntegrity(t *testing.T) {
	path := snapshotFixture(t, func(db *sql.DB) {
		execFixture(t, db, `PRAGMA ignore_check_constraints=ON`)
		seedMessage(t, db, "self", "self", nil, nil)
		seedMessage(t, db, "cycle-a", "cycle-b", nil, nil)
		seedMessage(t, db, "cycle-b", "cycle-a", nil, nil)
		seedMessage(t, db, "orphan", "absent", nil, nil)
		seedMessage(t, db, "ack", nil, nil, "2026-01-01T00:02:00Z")
		seedMessage(t, db, "bad-time", nil, nil, nil)
		execFixture(t, db, `UPDATE inbox_messages SET created_at='unknown-original-time' WHERE id='bad-time'`)
	})
	before := sourceBytes(t, path)
	got, err := InspectSnapshot(t.Context(), path, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	checkConserved(t, path, before)
	if got.Messages.Replies != 4 || got.Messages.OrphanReplies != 1 || got.Messages.SelfReplies != 1 || got.Messages.CyclicReplyMessages != 3 || got.Messages.AckWithoutSeen != 1 || got.Messages.InvalidTimestamps != 1 {
		t.Fatal("reply/handling blockers missing", got.Messages)
	}
}
func TestInspectSnapshotBoundsNeverTruncate(t *testing.T) {
	path := snapshotFixture(t, func(db *sql.DB) {
		seedJob(t, db)
		execFixture(t, db, `INSERT INTO job_briefs VALUES('private-job-id',1,'2026-01-01T00:00:00Z',?)`, strings.Repeat("x", 1000))
		seedMessage(t, db, "root", nil, nil, nil)
		seedMessage(t, db, "reply", "root", nil, nil)
	})
	before := sourceBytes(t, path)
	for _, limits := range []Limits{{MaxDatabaseBytes: 1}, {MaxRowsPerTable: 1}, {MaxTotalRows: 1}, {MaxCellBytes: 100}, {MaxTotalCellBytes: 100}} {
		_, err := InspectSnapshot(t.Context(), path, limits)
		inspectError(t, err, "snapshot_too_large", path)
		checkConserved(t, path, before)
	}
	_, err := InspectSnapshot(t.Context(), path, Limits{MaxRowsPerTable: -1})
	inspectError(t, err, "limits_invalid", path)
	_, err = InspectSnapshot(t.Context(), path, Limits{MaxDatabaseBytes: int64(len(before)), MaxRowsPerTable: 10, MaxTotalRows: 100, MaxCellBytes: 2000, MaxTotalCellBytes: 10000})
	if err != nil {
		t.Fatal("explicit sufficient bounds refused", err)
	}
}
func TestInspectSnapshotSealAndFileRefusals(t *testing.T) {
	path := snapshotFixture(t, nil)
	before := sourceBytes(t, path)
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.WriteFile(path+suffix, nil, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := InspectSnapshot(t.Context(), path, Limits{})
		inspectError(t, err, "snapshot_unsealed", path)
		if err = os.Remove(path + suffix); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(t.TempDir(), "selected-link")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		_, err := InspectSnapshot(t.Context(), link, Limits{})
		inspectError(t, err, "snapshot_invalid", path)
		if err = os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		_, err = InspectSnapshot(t.Context(), path, Limits{})
		inspectError(t, err, "snapshot_invalid", path)
		if err = os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []string{"relative.sqlite", t.TempDir(), filepath.Join(t.TempDir(), "absent")} {
		_, err := InspectSnapshot(t.Context(), bad, Limits{})
		inspectError(t, err, "snapshot_invalid", bad)
	}
	checkConserved(t, path, before)
}
func TestInspectSnapshotUnsupportedSchemaAndCancellation(t *testing.T) {
	for _, change := range []string{`DROP TABLE job_updates`, `UPDATE job_metadata SET jobs_schema_version=99`, `CREATE VIEW unrelated_private_view AS SELECT body FROM inbox_messages`, `ALTER TABLE job_notes ADD COLUMN unknown_private_column TEXT`} {
		path := snapshotFixture(t, func(db *sql.DB) { execFixture(t, db, change) })
		before := sourceBytes(t, path)
		_, err := InspectSnapshot(t.Context(), path, Limits{})
		inspectError(t, err, "snapshot_schema_unsupported", path, "unrelated_private_view", "unknown_private_column")
		checkConserved(t, path, before)
	}
	path := filepath.Join(t.TempDir(), "unrelated.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	execFixture(t, db, `CREATE TABLE arbitrary_secrets(secret TEXT)`)
	execFixture(t, db, `INSERT INTO arbitrary_secrets VALUES('private-unrelated-content')`)
	db.Close()
	os.Chmod(path, 0600)
	_, err = InspectSnapshot(t.Context(), path, Limits{})
	inspectError(t, err, "snapshot_schema_unsupported", path, "private-unrelated-content", "arbitrary_secrets")
	path = snapshotFixture(t, nil)
	before := sourceBytes(t, path)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = InspectSnapshot(ctx, path, Limits{})
	inspectError(t, err, "inspection_interrupted", path)
	expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	_, err = InspectSnapshot(expired, path, Limits{})
	inspectError(t, err, "inspection_interrupted", path)
	checkConserved(t, path, before)
}

func TestInspectSnapshotCoreOnlyAndInvalidUTF8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	execFixture(t, db, `PRAGMA user_version=1; CREATE TABLE controller_metadata(singleton INTEGER PRIMARY KEY,data_model_version INTEGER,min_writer_build INTEGER); CREATE TABLE review_state(singleton INTEGER PRIMARY KEY,state_json TEXT); INSERT INTO controller_metadata VALUES(1,1,1); INSERT INTO review_state VALUES(1,'{"schema_version":1}')`)
	db.Close()
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	before := sourceBytes(t, path)
	got, err := InspectSnapshot(t.Context(), path, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	checkConserved(t, path, before)
	if len(got.Counts) != 2 || got.Messages.HandlingTimestampsAvailable || got.Versions.Controller != 1 || got.Versions.Jobs != 0 {
		t.Fatal("absent optional source families invented availability")
	}
	path = snapshotFixture(t, func(db *sql.DB) {
		seedJob(t, db)
		execFixture(t, db, `INSERT INTO job_briefs VALUES('private-job-id',1,'2026-01-01T00:00:00Z',?)`, []byte{0xff, 0xfe})
	})
	got, err = InspectSnapshot(t.Context(), path, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Briefs.InvalidText != 1 || got.Briefs.MaxUTF8Bytes != 2 {
		t.Fatal("invalid UTF8 source body was repaired or ignored")
	}
}
