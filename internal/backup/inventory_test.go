package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviews"
)

func resealBundle(t *testing.T, path string, edit func(*Manifest)) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(path, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if json.Unmarshal(body, &m) != nil {
		t.Fatal("manifest")
	}
	if edit != nil {
		edit(&m)
	}
	actual, err := hashPrivate(t.Context(), filepath.Join(path, "firecontroller.sqlite"), maxDatabaseBytes)
	if err != nil {
		t.Fatal(err)
	}
	m.Database = actual
	body, err = canonicalManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if os.WriteFile(filepath.Join(path, "manifest.json"), body, 0600) != nil || os.WriteFile(filepath.Join(path, "manifest.sha256"), []byte(hex.EncodeToString(sum[:])+"\n"), 0600) != nil {
		t.Fatal("reseal fixture")
	}
}
func mutateBundleSQL(t *testing.T, path, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(path, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(query, args...); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	resealBundle(t, path, nil)
}
func TestInventorySnapshotSchemaCompatibilityWithoutMigration(t *testing.T) {
	// A legitimate core-only installation has no optional domain tables.
	dir := filepath.Join(t.TempDir(), "plain")
	store, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	created, err := Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal(err)
	}
	if created.ArtifactCount != 0 {
		t.Fatal(created)
	}
	if _, err = Inspect(t.Context(), created.Path); err != nil {
		t.Fatal("plain coordination rejected", err)
	}
	// Reconstruct known jobs v1 in this disposable fixture, then prove inspection
	// accepts its truthful old metadata without adding the v2 checkpoint table.
	if err = jobs.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().Exec(`DROP TABLE note_revisions; DROP TABLE notes; DROP TABLE worker_pause_history; DROP TABLE worker_pause_state; DROP TABLE worker_primary_job_history; DROP TABLE worker_primary_jobs; DROP TABLE job_updates; DROP TABLE job_checklists; DROP TABLE job_checklist_history; DROP TABLE job_checkpoints; UPDATE metadata SET value='1' WHERE key='schema:jobs'`); err != nil {
		t.Fatal(err)
	}
	created, err = Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal(err)
	}
	before := restoreTree(t, created.Path)
	if _, err = Inspect(t.Context(), created.Path); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, restoreTree(t, created.Path)) {
		t.Fatal("old schema migrated during inspection")
	}
	for _, change := range []string{`PRAGMA user_version=3`, `UPDATE metadata SET value='99' WHERE key='schema:jobs'`, `INSERT INTO metadata(key,value) VALUES('schema:unknown_domain','1')`, `UPDATE metadata SET value='2' WHERE key='schema:jobs'`, `DELETE FROM metadata WHERE key='schema:jobs'`, `INSERT INTO metadata(key,value) VALUES('restore:state','inactive')`} {
		t.Run(change, func(t *testing.T) {
			fresh, err := Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatal(err)
			}
			mutateBundleSQL(t, fresh.Path, change)
			_, err = Inspect(t.Context(), fresh.Path)
			backupCode(t, err, "backup_invalid")
		})
	}
}
func TestInventoryManifestHashPathPrivacyAndCompletenessFailures(t *testing.T) {
	for _, kind := range []string{"checksum", "path", "hash", "null_artifacts", "extra_file", "missing_marker", "symlink", "public_mode", "missing_database"} {
		t.Run(kind, func(t *testing.T) {
			f := newRestoreFixture(t)
			s := restoreBundle(t, f)
			switch kind {
			case "checksum":
				os.WriteFile(filepath.Join(s.Path, "manifest.sha256"), []byte(strings.Repeat("0", 64)+"\n"), 0600)
			case "path":
				resealBundle(t, s.Path, func(m *Manifest) { m.Artifacts[0].Path = "../outside" })
			case "hash":
				resealBundle(t, s.Path, func(m *Manifest) { m.Artifacts[0].SHA256 = strings.Repeat("0", 64) })
			case "null_artifacts":
				resealBundle(t, s.Path, func(m *Manifest) { m.Artifacts = nil })
			case "extra_file":
				os.WriteFile(filepath.Join(s.Path, "private-token"), []byte("must not be in bundle"), 0600)
			case "missing_marker":
				os.Remove(filepath.Join(s.Path, "manifest.sha256"))
			case "symlink":
				os.Remove(filepath.Join(s.Path, "manifest.json"))
				os.Symlink(filepath.Join(f.dir, "bootstrap-admin.token"), filepath.Join(s.Path, "manifest.json"))
			case "public_mode":
				os.Chmod(filepath.Join(s.Path, "firecontroller.sqlite"), 0644)
			case "missing_database":
				os.Remove(filepath.Join(s.Path, "firecontroller.sqlite"))
			}
			_, err := Inspect(t.Context(), s.Path)
			backupCode(t, err, "backup_invalid")
			if strings.Contains(err.Error(), s.Path) || strings.Contains(err.Error(), f.token) {
				t.Fatal("diagnostic leaked source")
			}
		})
	}
}
func TestInventoryReferencesCannotBeResealedAroundMissingOrChangedRawBytes(t *testing.T) {
	for _, kind := range []string{"missing_cli", "changed_cli", "changed_ref_bytes", "changed_ref_hash", "hosted_index_disagrees", "foreign_key", "custom_trigger", "obsolete_canonical_trigger"} {
		t.Run(kind, func(t *testing.T) {
			f := newRestoreFixture(t)
			cli, hosted := bundleNativeReferences(t, f)
			_ = hosted
			s := restoreBundle(t, f)
			switch kind {
			case "missing_cli":
				os.Remove(filepath.Join(s.Path, filepath.FromSlash(cli)))
				resealBundle(t, s.Path, func(m *Manifest) {
					var keep []Artifact
					for _, a := range m.Artifacts {
						if a.Path != cli {
							keep = append(keep, a)
						}
					}
					m.Artifacts = keep
				})
			case "changed_cli":
				p := filepath.Join(s.Path, filepath.FromSlash(cli))
				os.WriteFile(p, []byte("changed raw capture"), 0600)
				resealBundle(t, s.Path, func(m *Manifest) {
					for i, a := range m.Artifacts {
						if a.Path == cli {
							actual, err := hashPrivate(t.Context(), p, maxCLICaptureBytes)
							if err != nil {
								t.Fatal(err)
							}
							actual.Path = cli
							m.Artifacts[i] = actual
						}
					}
				})
			case "changed_ref_bytes":
				mutateBundleSQL(t, s.Path, `UPDATE review_execution_evidence SET body_json=json_set(body_json,'$.capture.capture_bytes',999)`)
			case "changed_ref_hash":
				mutateBundleSQL(t, s.Path, `UPDATE review_execution_evidence SET fingerprint=?`, strings.Repeat("0", 64))
			case "hosted_index_disagrees":
				mutateBundleSQL(t, s.Path, `UPDATE review_hosted_observations SET capture_bytes=capture_bytes+1`)
			case "foreign_key":
				mutateBundleSQL(t, s.Path, `UPDATE jobs SET project_id='nonexistent-project'`)
			case "custom_trigger":
				mutateBundleSQL(t, s.Path, `CREATE TRIGGER corrupt_restore AFTER UPDATE ON credentials BEGIN UPDATE jobs SET brief='corrupted'; END`)
			case "obsolete_canonical_trigger":
				mutateBundleSQL(t, s.Path, `CREATE TRIGGER review_runs_snapshot_revision_insert INSTEAD OF INSERT ON review_runs BEGIN SELECT 1; END`)
			}
			_, err := Inspect(t.Context(), s.Path)
			backupCode(t, err, "backup_invalid")
		})
	}
}
func TestInventorySourceSymlinkNonregularLimitsAndHostedFilenameHash(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo_like_directory", "oversize_cli", "hosted_wrong_hash", "directory_symlink", "custom_trigger"} {
		t.Run(kind, func(t *testing.T) {
			f := newRestoreFixture(t)
			dir := filepath.Join(f.dir, "review-captures")
			name := core.NewID("review_attempt") + ".json"
			switch kind {
			case "symlink":
				os.Symlink(filepath.Join(f.dir, "bootstrap-admin.token"), filepath.Join(dir, name))
			case "fifo_like_directory":
				os.Mkdir(filepath.Join(dir, name), 0700)
			case "oversize_cli":
				file, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if file.Truncate(maxCLICaptureBytes+1) != nil {
					t.Fatal("truncate")
				}
				file.Close()
			case "hosted_wrong_hash":
				os.WriteFile(filepath.Join(f.dir, "hosted-review-captures", core.NewID("review_attempt")+"-hosted-"+strings.Repeat("0", 64)+".json"), []byte("raw forensic bytes"), 0600)
			case "directory_symlink":
				os.Rename(dir, dir+"-owned-original")
				os.Symlink(dir+"-owned-original", dir)
			case "custom_trigger":
				if _, err := f.store.DB().Exec(`CREATE TRIGGER unsupported_source_trigger AFTER UPDATE ON credentials BEGIN SELECT 1; END`); err != nil {
					t.Fatal(err)
				}
			}
			root := filepath.Join(t.TempDir(), "bundles")
			_, err := Create(t.Context(), f.store, f.dir, root)
			backupCode(t, err, "backup_invalid")
			entries, readErr := os.ReadDir(root)
			if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatal("failed bundle published")
			}
		})
	}
}
func TestInventoryCopyPrivateNeverReplacesAndCanceledInspectIsReadOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private")
	if os.Mkdir(root, 0700) != nil {
		t.Fatal("mkdir")
	}
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "existing")
	os.WriteFile(source, []byte("owned source"), 0600)
	os.WriteFile(target, []byte("old destination"), 0600)
	_, err := copyPrivate(t.Context(), source, target, 1024)
	backupCode(t, err, "backup_exists")
	body, _ := os.ReadFile(target)
	if string(body) != "old destination" {
		t.Fatal("destination replaced")
	}
	f := newRestoreFixture(t)
	s := restoreBundle(t, f)
	before := restoreTree(t, s.Path)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = Inspect(ctx, s.Path)
	backupCode(t, err, "backup_unavailable")
	if !reflect.DeepEqual(before, restoreTree(t, s.Path)) {
		t.Fatal("canceled inspect changed bundle")
	}
}

func TestInventoryRejectsRehashedForeignKeyCascadeBeforeRestore(t *testing.T) {
	f := newRestoreFixture(t)
	bundle := restoreBundle(t, f)
	// Keep every visible jobs column and ordinary FK, but add a composite update
	// cascade to a metadata value rotated by Restore. Integrity/FK data checks
	// alone pass because this fixture starts in a referentially valid state.
	mutateBundleSQL(t, bundle.Path, `
 PRAGMA foreign_keys=OFF;
 CREATE UNIQUE INDEX malicious_metadata_pair ON metadata(key,value);
 CREATE TABLE altered_jobs (
 project_id TEXT NOT NULL,id TEXT NOT NULL,title TEXT NOT NULL,brief TEXT NOT NULL,worker_id TEXT,
 status TEXT NOT NULL CHECK(status IN ('active','parked','blocked','completed')),
 revision INTEGER NOT NULL CHECK(revision>=1),created_at TEXT NOT NULL,updated_at TEXT NOT NULL,
 PRIMARY KEY(project_id,id),
 FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE,
 FOREIGN KEY(project_id,worker_id) REFERENCES workers(project_id,id),
 FOREIGN KEY(title,brief) REFERENCES metadata(key,value) ON UPDATE CASCADE);
 INSERT INTO altered_jobs SELECT project_id,id,'signing_key',(SELECT value FROM metadata WHERE key='signing_key'),worker_id,status,revision,created_at,updated_at FROM jobs;
 DROP TABLE jobs;
 ALTER TABLE altered_jobs RENAME TO jobs;`)
	before := restoreTree(t, bundle.Path)
	_, inspectErr := Inspect(t.Context(), bundle.Path)
	if inspectErr == nil {
		t.Error("rehashed noncanonical FK cascade was accepted by inspection")
	} else {
		backupCode(t, inspectErr, "backup_invalid")
	}
	destination := filepath.Join(t.TempDir(), "restore-must-not-start")
	restored, err := Restore(t.Context(), bundle.Path, destination)
	if err == nil {
		db, openErr := openSnapshot(t.Context(), filepath.Join(restored.Destination, "firecontroller.sqlite"))
		if openErr != nil {
			t.Fatal(openErr)
		}
		var brief, key string
		readErr := db.QueryRow(`SELECT brief,(SELECT value FROM metadata WHERE key='signing_key') FROM jobs LIMIT 1`).Scan(&brief, &key)
		db.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if brief == key {
			t.Error("unsupported FK reached restore identity rotation and cascaded into retained job brief")
		}
		t.Fatal("Restore accepted a noncanonical FK definition")
	}
	backupCode(t, err, "backup_invalid")
	if _, err = os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("invalid schema reached destination writes")
	}
	if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) {
		t.Fatal("rejection changed retained bundle data")
	}
}

func TestInventoryRejectsChangedMissingDuplicateForeignKeysAndGeneratedColumns(t *testing.T) {
	for _, change := range []string{"update_action", "delete_action", "missing", "duplicate", "generated"} {
		t.Run(change, func(t *testing.T) {
			f := newRestoreFixture(t)
			bundle := restoreBundle(t, f)
			create := `CREATE TABLE altered_jobs (
 project_id TEXT NOT NULL,id TEXT NOT NULL,title TEXT NOT NULL,brief TEXT NOT NULL,worker_id TEXT,
 status TEXT NOT NULL CHECK(status IN ('active','parked','blocked','completed')),
 revision INTEGER NOT NULL CHECK(revision>=1),created_at TEXT NOT NULL,updated_at TEXT NOT NULL,
 PRIMARY KEY(project_id,id),
 FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE,
 FOREIGN KEY(project_id,worker_id) REFERENCES workers(project_id,id));`
			switch change {
			case "update_action":
				create = strings.Replace(create, "REFERENCES projects(id) ON DELETE CASCADE", "REFERENCES projects(id) ON UPDATE CASCADE ON DELETE CASCADE", 1)
			case "delete_action":
				create = strings.Replace(create, "ON DELETE CASCADE", "ON DELETE SET NULL", 1)
			case "missing":
				create = strings.Replace(create, "FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE,", "", 1)
			case "duplicate":
				create = strings.Replace(create, "PRIMARY KEY(project_id,id),", "PRIMARY KEY(project_id,id), FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE,", 1)
			case "generated":
				create = strings.Replace(create, "PRIMARY KEY(project_id,id),", "ignored_generated TEXT GENERATED ALWAYS AS (brief) VIRTUAL, PRIMARY KEY(project_id,id),", 1)
			}
			mutateBundleSQL(t, bundle.Path, `PRAGMA foreign_keys=OFF;`+create+`INSERT INTO altered_jobs(project_id,id,title,brief,worker_id,status,revision,created_at,updated_at) SELECT project_id,id,title,brief,worker_id,status,revision,created_at,updated_at FROM jobs; DROP TABLE jobs; ALTER TABLE altered_jobs RENAME TO jobs;`)
			_, err := Inspect(t.Context(), bundle.Path)
			backupCode(t, err, "backup_invalid")
		})
	}
}

// installLegacyReviewInventorySchema uses the three published physical migrations
// from b6e599270045e2fa87d81c294e08b2177cda3c1a. Build old bundles directly;
// never relabel version-four views/version tables as predecessor storage.
func installLegacyReviewInventorySchema(ctx context.Context, store *core.Store, version int) error {
	migrations := [][]string{{
		`CREATE TABLE IF NOT EXISTS review_snapshots (
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 snapshot_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK (revision >= 1),
 repository_id INTEGER NOT NULL CHECK (repository_id > 0),
 source TEXT NOT NULL,
 observed_at TEXT NOT NULL,
 recorded_at TEXT NOT NULL,
 payload_sha256 TEXT NOT NULL,
 PRIMARY KEY (project_id, snapshot_id),
 UNIQUE (project_id, revision)
);`,
		`CREATE TABLE IF NOT EXISTS review_workspace (
 project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL CHECK (revision >= 1),
 snapshot_id TEXT NOT NULL,
 FOREIGN KEY (project_id, snapshot_id) REFERENCES review_snapshots(project_id, snapshot_id)
);`,
		`CREATE TABLE IF NOT EXISTS review_pulls (
 project_id TEXT NOT NULL,
 snapshot_id TEXT NOT NULL,
 position INTEGER NOT NULL CHECK (position >= 0),
 number INTEGER NOT NULL CHECK (number > 0 AND number <= 2147483647),
 title TEXT NOT NULL,
 state TEXT NOT NULL,
 head_sha TEXT NOT NULL,
 base_ref TEXT NOT NULL,
 base_sha TEXT NOT NULL,
 ci_state TEXT NOT NULL,
 mergeability TEXT NOT NULL,
 complete INTEGER NOT NULL CHECK (complete IN (0,1)),
 run_count INTEGER NOT NULL CHECK (run_count >= 0),
 attempt_count INTEGER NOT NULL CHECK (attempt_count >= 0),
 recent_runs_json TEXT NOT NULL,
 latest_attempts_json TEXT NOT NULL,
 PRIMARY KEY (project_id, snapshot_id, number),
 UNIQUE (project_id, snapshot_id, position),
 FOREIGN KEY (project_id, snapshot_id) REFERENCES review_snapshots(project_id, snapshot_id) ON DELETE CASCADE
);`,
		`CREATE INDEX IF NOT EXISTS review_pulls_snapshot_position
 ON review_pulls(project_id, snapshot_id, position);`,
		`CREATE TABLE IF NOT EXISTS review_runs (
 project_id TEXT NOT NULL,
 snapshot_id TEXT NOT NULL,
 pull_number INTEGER NOT NULL,
 run_order INTEGER NOT NULL CHECK (run_order >= 0),
 id TEXT NOT NULL,
 source TEXT NOT NULL,
 source_run_id TEXT NOT NULL,
 channel TEXT NOT NULL,
 head_sha TEXT NOT NULL,
 reviewer TEXT NOT NULL,
 scope TEXT NOT NULL,
 outcome TEXT NOT NULL,
 attributable INTEGER NOT NULL CHECK (attributable IN (0,1)),
 complete INTEGER NOT NULL CHECK (complete IN (0,1)),
 started_at TEXT NOT NULL,
 finished_at TEXT NOT NULL,
 found INTEGER NOT NULL CHECK (found >= 0),
 accepted INTEGER NOT NULL CHECK (accepted >= 0),
 routed INTEGER NOT NULL CHECK (routed >= 0),
 finalized INTEGER NOT NULL CHECK (finalized IN (0,1)),
 fixed INTEGER NOT NULL CHECK (fixed >= 0),
	coverage_limits_json TEXT NOT NULL,
 PRIMARY KEY (project_id, snapshot_id, pull_number, id),
 UNIQUE (project_id, snapshot_id, pull_number, run_order),
 UNIQUE (project_id, snapshot_id, source, source_run_id),
 FOREIGN KEY (project_id, snapshot_id, pull_number)
  REFERENCES review_pulls(project_id, snapshot_id, number) ON DELETE CASCADE
);`,
		`CREATE INDEX IF NOT EXISTS review_runs_project_source_id
 ON review_runs(project_id, source, source_run_id, finalized);`,
		`CREATE INDEX IF NOT EXISTS review_runs_snapshot_pull_order
 ON review_runs(project_id, snapshot_id, pull_number, run_order DESC);`,
		`CREATE TABLE IF NOT EXISTS review_findings (
 project_id TEXT NOT NULL,
 snapshot_id TEXT NOT NULL,
 pull_number INTEGER NOT NULL,
 run_id TEXT NOT NULL,
 position INTEGER NOT NULL CHECK (position >= 0),
 source_finding_id TEXT NOT NULL,
 title TEXT NOT NULL,
 detail TEXT NOT NULL,
 severity TEXT NOT NULL,
 disposition TEXT NOT NULL,
 decision_reason TEXT NOT NULL,
 actor TEXT NOT NULL,
 decided_at TEXT NOT NULL,
 fix_sha TEXT NOT NULL,
 proof_note TEXT NOT NULL,
 resolved_at TEXT NOT NULL,
 PRIMARY KEY (project_id, snapshot_id, pull_number, run_id, source_finding_id),
 UNIQUE (project_id, snapshot_id, pull_number, run_id, position),
 FOREIGN KEY (project_id, snapshot_id, pull_number, run_id)
  REFERENCES review_runs(project_id, snapshot_id, pull_number, id) ON DELETE CASCADE
);`,
		`CREATE INDEX IF NOT EXISTS review_findings_run_position
 ON review_findings(project_id, snapshot_id, pull_number, run_id, position);`,
		`CREATE TABLE IF NOT EXISTS review_attempts (
 project_id TEXT NOT NULL,
 snapshot_id TEXT NOT NULL,
 pull_number INTEGER NOT NULL,
 position INTEGER NOT NULL CHECK (position >= 0),
 source TEXT NOT NULL,
 source_attempt_id TEXT NOT NULL,
 channel TEXT NOT NULL,
 head_sha TEXT NOT NULL,
 state TEXT NOT NULL,
 started_at TEXT NOT NULL,
 finished_at TEXT NOT NULL,
 duration_seconds INTEGER CHECK (duration_seconds IS NULL OR duration_seconds >= 0),
 source_run_id TEXT NOT NULL,
 trigger_id TEXT NOT NULL,
 provider_review_id TEXT NOT NULL,
 PRIMARY KEY (project_id, snapshot_id, source, source_attempt_id),
 UNIQUE (project_id, snapshot_id, pull_number, position),
 FOREIGN KEY (project_id, snapshot_id, pull_number)
  REFERENCES review_pulls(project_id, snapshot_id, number) ON DELETE CASCADE
);`,
		`CREATE INDEX IF NOT EXISTS review_attempts_snapshot_pull_position
 ON review_attempts(project_id, snapshot_id, pull_number, position);`,
		`CREATE TABLE IF NOT EXISTS review_retired_pulls (
 project_id TEXT NOT NULL,
 snapshot_id TEXT NOT NULL,
 position INTEGER NOT NULL CHECK (position >= 0),
 number INTEGER NOT NULL CHECK (number > 0 AND number <= 2147483647),
 reason TEXT NOT NULL CHECK (reason IN ('superseded','defunct')),
 PRIMARY KEY (project_id, snapshot_id, number),
 UNIQUE (project_id, snapshot_id, position),
 FOREIGN KEY (project_id, snapshot_id) REFERENCES review_snapshots(project_id, snapshot_id) ON DELETE CASCADE
);`,
	}, {
		`ALTER TABLE review_runs ADD COLUMN snapshot_revision INTEGER NOT NULL DEFAULT 0`,
		`UPDATE review_runs SET snapshot_revision=(SELECT revision FROM review_snapshots s WHERE s.project_id=review_runs.project_id AND s.snapshot_id=review_runs.snapshot_id)`,
		`DROP INDEX review_runs_project_source_id`,
		`CREATE INDEX review_runs_project_source_id ON review_runs(project_id,source,source_run_id,finalized,snapshot_revision DESC)`,
		`CREATE TRIGGER review_runs_snapshot_revision_insert BEFORE INSERT ON review_runs WHEN NEW.snapshot_revision IS NOT (SELECT revision FROM review_snapshots WHERE project_id=NEW.project_id AND snapshot_id=NEW.snapshot_id) BEGIN SELECT RAISE(ABORT,'invalid review snapshot revision'); END`,
		`CREATE TRIGGER review_runs_snapshot_revision_update BEFORE UPDATE OF project_id,snapshot_id,snapshot_revision ON review_runs WHEN NEW.snapshot_revision IS NOT (SELECT revision FROM review_snapshots WHERE project_id=NEW.project_id AND snapshot_id=NEW.snapshot_id) BEGIN SELECT RAISE(ABORT,'invalid review snapshot revision'); END`,
	}, {
		`ALTER TABLE review_findings ADD COLUMN route_json TEXT NOT NULL DEFAULT 'null'`,
	}}
	return store.EnsureSchema(ctx, "reviews", migrations[:version]...)
}

func TestInventoryCanonicalForeignKeysRemainSupportedAcrossOlderSchemas(t *testing.T) {
	cases := []struct {
		domain  string
		version int
	}{
		{"jobs", 1}, {"jobs", 2}, {"jobs", 3}, {"jobs", 4}, {"jobs", 5},
		{"reviews", 1}, {"reviews", 2}, {"reviews", 3},
		{"review_execution", 1}, {"review_execution", 2}, {"review_execution", 3}, {"review_execution", 4}, {"review_execution", 5},
		{"review_evidence", 1}, {"review_evidence", 2}, {"review_adjudication", 1},
		{"review_source_refresh", 1}, {"review_source_refresh", 2},
	}
	for _, test := range cases {
		t.Run(test.domain+"/"+string(rune('0'+test.version)), func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "source")
			store, err := core.Open(t.Context(), source)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			ensureReviews := reviews.EnsureSchema
			if test.domain == "reviews" {
				ensureReviews = func(ctx context.Context, store *core.Store) error {
					return installLegacyReviewInventorySchema(ctx, store, test.version)
				}
			}
			for _, ensure := range []func(context.Context, *core.Store) error{jobs.EnsureSchema, inbox.EnsureSchema, ensureReviews, reviewexec.EnsureSchema} {
				if err = ensure(t.Context(), store); err != nil {
					t.Fatal(err)
				}
			}
			var statements []string
			switch test.domain {
			case "jobs":
				statements = []string{`DROP TABLE note_revisions`, `DROP TABLE notes`}
				if test.version < 5 {
					statements = append(statements, `DROP TABLE worker_pause_history`, `DROP TABLE worker_pause_state`)
				}
				if test.version < 4 {
					statements = append(statements, `DROP TABLE worker_primary_job_history`, `DROP TABLE worker_primary_jobs`)
				}
				if test.version < 3 {
					statements = append(statements, `DROP TABLE job_updates`, `DROP TABLE job_checklists`, `DROP TABLE job_checklist_history`)
				}
				if test.version < 2 {
					statements = append(statements, `DROP TABLE job_checkpoints`)
				}
			case "review_execution":
				statements = []string{`DROP TABLE review_hosted_allowance_queries`, `DROP TABLE review_hosted_preflight`}
				if test.version < 4 {
					statements = append(statements, `DROP TABLE review_cli_preflight`)
				}
				if test.version < 3 {
					statements = append(statements, `ALTER TABLE review_operations DROP COLUMN policy_revision`)
				}
				if test.version < 2 {
					statements = append(statements, `ALTER TABLE review_operations DROP COLUMN capability_fingerprint`)
				}
			case "review_evidence":
				statements = []string{`DROP TABLE review_hosted_observations`}
				if test.version == 1 {
					statements = append(statements, `DROP INDEX review_evidence_operation`)
				}
			case "review_adjudication":
				statements = []string{`DROP TABLE review_finding_route_revisions`, `DROP TABLE review_finding_routes`}
			case "review_source_refresh":
				rows, e := store.DB().Query(`SELECT name FROM sqlite_schema WHERE type='index' AND tbl_name='review_source_refreshes' AND sql IS NOT NULL`)
				if e != nil {
					t.Fatal(e)
				}
				for rows.Next() {
					var name string
					if rows.Scan(&name) != nil {
						t.Fatal("index fixture")
					}
					statements = append(statements, `DROP INDEX "`+name+`"`)
				}
				rows.Close()
				statements = append(statements, `ALTER TABLE review_source_refreshes DROP COLUMN include_readiness`, `ALTER TABLE review_source_refreshes DROP COLUMN review_state_json`)
				if test.version == 1 {
					statements = append(statements, `ALTER TABLE review_source_refreshes DROP COLUMN include_ci`, `ALTER TABLE review_source_refreshes DROP COLUMN ci_observation_json`)
				}
			}
			for _, statement := range statements {
				if _, err = store.DB().Exec(statement); err != nil {
					t.Fatal(statement, err)
				}
			}
			if _, err = store.DB().Exec(`UPDATE metadata SET value=? WHERE key=?`, test.version, "schema:"+test.domain); err != nil {
				t.Fatal(err)
			}
			before := backupRows(t, store.DB())
			created, err := Create(t.Context(), store, source, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatal("supported canonical FK schema refused", err)
			}
			if _, err = Inspect(t.Context(), created.Path); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, backupRows(t, store.DB())) {
				t.Fatal("old source migrated by validation")
			}
		})
	}
}
