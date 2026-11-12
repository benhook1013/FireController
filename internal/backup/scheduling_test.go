package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type schedulingBackupReader struct {
	*selectionInventoryReader
	entered chan struct{}
	once    sync.Once
}

func (r *schedulingBackupReader) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	r.once.Do(func() { close(r.entered) })
	return r.selectionInventoryReader.Repository(ctx, id)
}

func schedulingBackupFixture(t *testing.T) (*restoreFixture, *reviewexec.Engine, string) {
	t.Helper()
	f := newRestoreFixture(t)
	e := selectionInventoryEngine(t, f.store)
	scope := routePolicyInventoryScope(t, f)
	var attempt string
	var revision int64
	if err := f.store.DB().QueryRow(`SELECT id,revision FROM review_execution_attempts`).Scan(&attempt, &revision); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Advance(t.Context(), f.project, attempt, revision, "held", "synthetic_preparation_ended", ""); err != nil {
		t.Fatal(err)
	}
	one, zero, on := int64(1), int64(0), true
	if _, err := e.Configure(t.Context(), scope, "scheduling-backup-queue", reviewexec.Configuration{ExpectedRevision: &one, CapabilityID: "restore-cap", Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"hosted"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ConfigurePullScheduling(t.Context(), scope, "pause-backup-pull", 2, reviewexec.PullSchedulingRequest{ExpectedRevision: &zero, Paused: &on, Reason: "Fix in progress"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ConfigureScheduling(t.Context(), scope, "enable-backup-scheduling", reviewexec.SchedulingRequest{ExpectedRevision: &zero, AutomaticHosted: &on}); err != nil {
		t.Fatal(err)
	}
	reader := &schedulingBackupReader{selectionInventoryReader: &selectionInventoryReader{state: "selected"}, entered: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	done, err := e.StartReviewRuns(ctx, map[string]reviewexec.TargetReader{"restore-cap": reader})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(cancel)
	select {
	case <-reader.entered:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("automatic selection did not read synthetic source")
	}
	var selection string
	if err := f.store.DB().QueryRow(`SELECT selection_id FROM review_automatic_selections`).Scan(&selection); err != nil {
		cancel()
		t.Fatal(err)
	}
	run, terminal, err := e.WaitRun(t.Context(), scope, selection, 3)
	cancel()
	select {
	case workerErr := <-done:
		if workerErr != nil {
			t.Fatal(workerErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("selection worker did not join")
	}
	if err != nil || !terminal || run.State != "selected" || run.OperationID == "" {
		t.Fatal("automatic selection did not retain a native child", run, err)
	}
	if err := f.store.DB().QueryRow(`SELECT credential_id FROM review_scheduling WHERE project_id=?`, f.project).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	return f, e, attempt
}

func schedulingBackupRows(t *testing.T, db *sql.DB) map[string][][]any {
	t.Helper()
	rows := map[string][][]any{}
	for _, table := range []string{"review_scheduling", "review_pull_scheduling", "review_automatic_selections"} {
		rows[table] = restoreRows(t, db, `SELECT * FROM `+table+` ORDER BY 1`)
	}
	return rows
}

func TestSchedulingBackupRetainsNativeControlsAndInactiveRestore(t *testing.T) {
	f, _, credential := schedulingBackupFixture(t)
	before := schedulingBackupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive-scheduling")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	restored, err := core.Open(t.Context(), destination)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if !reflect.DeepEqual(before, schedulingBackupRows(t, restored.DB())) || !reflect.DeepEqual(before, schedulingBackupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("backup/inspect/restore changed retained scheduling facts")
	}
	status, err := ReadRecoveryStatus(t.Context(), restored)
	if err != nil || status.State != "inactive" || !status.ProvidersHeld {
		t.Fatal("restore enabled provider work", status, err)
	}
	backupCode(t, StartupGuard(t.Context(), restored), "restore_inactive")
	tx, err := restored.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := reviewexec.ValidateSchedulingSnapshot(t.Context(), tx); err != nil {
		t.Fatal("revoked restored authors lost historical validity", err)
	}
	_, err = restored.ProjectAutomationScope(t.Context(), tx, f.project, f.principal, f.worker, credential)
	backupCode(t, err, "forbidden")
}

func TestSchedulingBackupHistoricalAuthorizersRemainHistory(t *testing.T) {
	f, _, credential := schedulingBackupFixture(t)
	for _, kind := range []string{"revoked", "demoted", "retired"} {
		t.Run(kind, func(t *testing.T) {
			var query string
			switch kind {
			case "revoked":
				query = `UPDATE credentials SET revoked=1 WHERE id=?`
			case "demoted":
				query = `UPDATE credentials SET is_admin=0 WHERE id=?`
			case "retired":
				query = `INSERT INTO worker_retirements(project_id,worker_id,retired_at,actor_id) VALUES('` + f.project + `','` + f.worker + `','2026-11-01T00:00:00Z','synthetic-actor')`
			}
			if kind == "retired" {
				if _, err := f.store.DB().Exec(query); err != nil {
					t.Fatal(err)
				}
			} else if _, err := f.store.DB().Exec(query, credential); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=0,is_admin=1 WHERE id=?; DELETE FROM worker_retirements WHERE project_id=? AND worker_id=?`, credential, f.project, f.worker); err != nil {
					t.Fatal(err)
				}
			})
			tx, err := f.store.DB().BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.store.ProjectAutomationScope(t.Context(), tx, f.project, f.principal, f.worker, credential)
			_ = tx.Rollback()
			if err == nil {
				t.Fatal("historical authorizer still authorized new automation")
			}
			before := schedulingBackupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal("valid history refused", err)
			}
			dest := filepath.Join(t.TempDir(), "historical-inactive")
			if _, err := Restore(t.Context(), bundle.Path, dest); err != nil {
				t.Fatal(err)
			}
			db, err := openSnapshot(t.Context(), filepath.Join(dest, "firecontroller.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if !reflect.DeepEqual(before, schedulingBackupRows(t, db)) {
				t.Fatal("restore rewrote historical scheduling attribution")
			}
		})
	}
}

func schedulingBundleCopy(t *testing.T, source string) string {
	t.Helper()
	destination := filepath.Join(t.TempDir(), filepath.Base(source))
	if err := os.CopyFS(destination, os.DirFS(source)); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(destination, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0600)
		if d.IsDir() {
			mode = 0700
		}
		return os.Chmod(p, mode)
	}); err != nil {
		t.Fatal(err)
	}
	return destination
}

func TestSchedulingBackupRefusesResealedShapeReferencesAndProvenance(t *testing.T) {
	f, _, _ := schedulingBackupFixture(t)
	bundle := restoreBundle(t, f)
	for _, tc := range []struct{ name, query string }{
		{"empty-current-event", `UPDATE events SET payload_json='{}' WHERE event_type='review.scheduling_updated'`},
		{"mismatched-current-event", `UPDATE events SET payload_json='{"automatic_hosted":false}' WHERE event_type='review.scheduling_updated'`},
		{"invalid-current-basis", `UPDATE events SET payload_json='{"automatic_hosted":true,"hosted_basis_revision":999,"cli_basis_revision":999}' WHERE event_type='review.scheduling_updated'`},
		{"missing-current-event", `DELETE FROM events WHERE event_type='review.scheduling_updated'`},
		{"wrong-current-event-actor", `UPDATE events SET actor_id='principal_missing' WHERE event_type='review.scheduling_updated'`},
		{"missing-domain", `DELETE FROM metadata WHERE key='schema:review_scheduling'`},
		{"missing-index", `DROP INDEX review_automatic_selections_pull`},
		{"weakened-schema", `PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql=replace(sql,'revision>0','revision>=0') WHERE name='review_scheduling'; PRAGMA writable_schema=OFF`},
		{"missing-worker", `UPDATE review_scheduling SET worker_id='worker_missing'`},
		{"forged-author", `UPDATE review_scheduling SET actor_id='principal_missing'`},
		{"orphan-selection", `UPDATE review_automatic_selections SET selection_id='review_selection_` + strings.Repeat("a", 32) + `'`},
		{"missing-credential", `UPDATE review_automatic_selections SET credential_id='credential_missing'`},
		{"wrong-target", `UPDATE review_automatic_selections SET pull_number=99`},
		{"future-control-revision", `UPDATE review_automatic_selections SET scheduling_revision=99`},
		{"blob-pause-reason", `UPDATE review_pull_scheduling SET reason=CAST(reason AS BLOB)`},
		{"blob-pause-actor", `UPDATE review_pull_scheduling SET actor_id=CAST(actor_id AS BLOB)`},
		{"blob-pause-time", `UPDATE review_pull_scheduling SET updated_at=CAST(updated_at AS BLOB)`},
		{"blob-control-time", `UPDATE review_scheduling SET updated_at=CAST(updated_at AS BLOB)`},
		{"blob-attribution-basis", `UPDATE review_automatic_selections SET basis_sha256=CAST(basis_sha256 AS BLOB)`},
		{"blob-attribution-settled", `UPDATE review_automatic_selections SET settled_basis_sha256=CAST(settled_basis_sha256 AS BLOB)`},
		{"blob-attribution-pull-basis", `UPDATE review_automatic_selections SET pull_basis_sha256=CAST(pull_basis_sha256 AS BLOB)`},
		{"invalid-basis", `UPDATE review_automatic_selections SET basis_sha256='` + strings.Repeat("z", 64) + `'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := schedulingBundleCopy(t, bundle.Path)
			mutateBundleSQL(t, copy, tc.query)
			_, err := Inspect(t.Context(), copy)
			backupCode(t, err, "backup_invalid")
			dest := filepath.Join(t.TempDir(), "refused")
			_, err = Restore(t.Context(), copy, dest)
			backupCode(t, err, "backup_invalid")
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("malformed scheduling created a restore destination", err)
			}
		})
	}
	// A genuine predecessor may omit the entire optional domain. Startup adds
	// empty controls only, never inferring enabled automation from old history.
	old := schedulingBundleCopy(t, bundle.Path)
	mutateBundleSQL(t, old, `DROP TABLE review_automatic_selections; DROP TABLE review_pull_scheduling; DROP TABLE review_scheduling; DELETE FROM metadata WHERE key='schema:review_scheduling'`)
	if _, err := Inspect(t.Context(), old); err != nil {
		t.Fatal("optional absent domain refused", err)
	}
	dest := filepath.Join(t.TempDir(), "old-inactive")
	if _, err := Restore(t.Context(), old, dest); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), dest)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := reviewexec.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM review_scheduling`).Scan(&count); err != nil || count != 0 {
		t.Fatal("old bundle startup enabled automation", count, err)
	}
}

func TestSchedulingCLIBackupAndV1Compatibility(t *testing.T) {
	f, e, _ := schedulingBackupFixture(t)
	one := int64(1)
	on := true
	if _, err := e.ConfigureScheduling(t.Context(), routePolicyInventoryScope(t, f), "enable-cli-backup", reviewexec.SchedulingRequest{ExpectedRevision: &one, AutomaticHosted: &on, AutomaticCLI: &on}); err != nil {
		t.Fatal(err)
	}
	bundle := restoreBundle(t, f)
	var err error
	if _, err = Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("CLI scheduling backup rejected", err)
	}
	old := schedulingBundleCopy(t, bundle.Path)
	mutateBundleSQL(t, old, `ALTER TABLE review_scheduling DROP COLUMN automatic_cli; UPDATE metadata SET value='1' WHERE key='schema:review_scheduling'; UPDATE events SET payload_json='{"automatic_hosted":true}' WHERE event_type='review.scheduling_updated'`)
	if _, err = Inspect(t.Context(), old); err != nil {
		t.Fatal("v1 scheduling backup rejected", err)
	}
	dest := filepath.Join(t.TempDir(), "v1-inactive")
	if _, err = Restore(t.Context(), old, dest); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), dest)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = reviewexec.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	var cli, hosted bool
	if err = store.DB().QueryRow(`SELECT automatic_cli,automatic_hosted FROM review_scheduling WHERE project_id=?`, f.project).Scan(&cli, &hosted); err != nil || cli || !hosted {
		t.Fatal("migration changed opt-in", cli, hosted, err)
	}
}
