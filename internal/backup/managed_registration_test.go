package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func TestManagedRegistrationSettlesAfterCreationCancellation(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m.wait = func(context.Context, time.Duration) error { return nil }
	create := m.create
	var published createdBundle
	m.create = func(ctx context.Context, s *core.Store, source, destination string) (createdBundle, error) {
		result, err := create(ctx, s, source, destination)
		if err == nil {
			published = result
			cancel()
		}
		return result, err
	}
	var reports []error
	if err := m.Run(ctx, func(err error) { reports = append(reports, err) }); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 {
		t.Fatal("cancelled retention outcome", reports)
	}
	backupCode(t, reports[0], "backup_retention_failed")
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil || status.LastSuccess == nil || *status.LastSuccess != published.Summary || status.LastAttempt == nil || status.LastAttempt.State != "complete" || status.UnregisteredBundle != nil || m.publicationHeld {
		t.Fatal("published backup lost registration after cancellation", status, err)
	}
	var state, hash string
	if err := f.store.DB().QueryRow(`SELECT state,manifest_sha256 FROM managed_backup_bundles WHERE id=?`, published.ID).Scan(&state, &hash); err != nil || state != "retained" || hash != published.hash {
		t.Fatal("cancelled creation did not settle exact ownership", state, hash, err)
	}
	if _, err := Inspect(t.Context(), published.Path); err != nil {
		t.Fatal("registered publication is not usable", err)
	}
}

// Install the temporary fixture trigger on Store's actual writer connection.
// It aborts ownership insertion without touching publication or source schema.
func managedRegistrationRejectInserts(t *testing.T, m *Manager) func() {
	t.Helper()
	if err := m.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TEMP TRIGGER reject_managed_registration BEFORE INSERT ON main.managed_backup_bundles BEGIN SELECT RAISE(ABORT,'fixture registration rollback'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := m.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS temp.reject_managed_registration`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestManagedRegistrationRollbackPreservesPublicationAndHoldsDestination(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	if reports := runManaged(t, m, 1); len(reports) != 0 {
		t.Fatal(reports)
	}
	prior, err := ReadStatus(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	create := m.create
	var published createdBundle
	var files map[string]string
	var removeTrigger func()
	creates := 0
	m.create = func(ctx context.Context, s *core.Store, source, destination string) (createdBundle, error) {
		creates++
		result, err := create(ctx, s, source, destination)
		if err == nil {
			published, files = result, restoreTree(t, result.Path)
			removeTrigger = managedRegistrationRejectInserts(t, m)
		}
		return result, err
	}
	reports := runManaged(t, m, 3)
	if removeTrigger != nil {
		removeTrigger()
	}
	if creates != 1 || len(reports) != 1 {
		t.Fatal("registration failure kept creating copies", creates, reports)
	}
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil || status.State != "held" || status.NextAttemptAt != "" || !reflect.DeepEqual(status.LastSuccess, prior.LastSuccess) || status.UnregisteredBundle == nil || *status.UnregisteredBundle != published.Summary || status.LastAttempt == nil || status.LastAttempt.State != "failed" || status.LastAttempt.Reason != "backup_registration_failed" {
		t.Fatal("unregistered publication not reported separately from success", status, err)
	}
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM managed_backup_bundles WHERE id=?`, published.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("fixture did not roll back ownership registration", count, err)
	}
	if !reflect.DeepEqual(files, restoreTree(t, published.Path)) {
		t.Fatal("registration rollback deleted published bytes")
	}
	if _, err := Inspect(t.Context(), published.Path); err != nil {
		t.Fatal("preserved publication is not usable", err)
	}
	oldTree := restoreTree(t, m.config.Destination)
	for _, disabled := range []bool{false, true, false} {
		cfg := m.config
		if disabled {
			cfg = ScheduleConfig{}
		}
		restarted, err := NewManager(f.store, f.dir, cfg)
		if err != nil {
			t.Fatal("restart refused retained registration evidence", err)
		}
		restarted.create = func(context.Context, *core.Store, string, string) (createdBundle, error) {
			t.Fatal("held/disabled restart created another copy")
			return createdBundle{}, nil
		}
		if reports := runManaged(t, restarted, 2); len(reports) != 0 {
			t.Fatal(reports)
		}
		current, err := ReadStatus(t.Context(), f.dir)
		wantState := "held"
		if disabled {
			wantState = "disabled"
		}
		if err != nil || current.State != wantState || current.UnregisteredBundle == nil || *current.UnregisteredBundle != published.Summary || !reflect.DeepEqual(current.LastSuccess, prior.LastSuccess) {
			t.Fatal("restart discarded registration hold", current, err)
		}
	}
	if !reflect.DeepEqual(oldTree, restoreTree(t, m.config.Destination)) {
		t.Fatal("restart changed held destination bytes")
	}
	cfg := m.config
	cfg.Destination = filepath.Join(t.TempDir(), "replacement-destination")
	replacement, err := NewManager(f.store, f.dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if reports := runManaged(t, replacement, 2); len(reports) != 0 {
		t.Fatal("new destination inherited old publication hold", reports)
	}
	current, err := ReadStatus(t.Context(), f.dir)
	if err != nil || current.State != "scheduled" || current.LastSuccess == nil || filepath.Dir(current.LastSuccess.Path) != cfg.Destination || current.UnregisteredBundle == nil || *current.UnregisteredBundle != published.Summary {
		t.Fatal("new destination discarded held evidence or failed", current, err)
	}
	if !reflect.DeepEqual(oldTree, restoreTree(t, m.config.Destination)) {
		t.Fatal("new destination touched old published files")
	}
}

func TestManagedRegistrationDoesNotAdoptOrDeleteReplacedDirectory(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	create := m.create
	var published createdBundle
	var original map[string]string
	var parked string
	m.create = func(ctx context.Context, s *core.Store, source, destination string) (createdBundle, error) {
		result, err := create(ctx, s, source, destination)
		if err != nil {
			return result, err
		}
		published, original = result, restoreTree(t, result.Path)
		parked = result.Path + ".preserved"
		if err := os.Rename(result.Path, parked); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(result.Path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(result.Path, "foreign-note"), []byte("preserve"), 0600); err != nil {
			t.Fatal(err)
		}
		return result, nil
	}
	if reports := runManaged(t, m, 2); len(reports) != 1 {
		t.Fatal("replacement was not held", reports)
	}
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil || status.State != "held" || status.LastSuccess != nil || status.UnregisteredBundle == nil || *status.UnregisteredBundle != published.Summary {
		t.Fatal("replacement acquired success credit", status, err)
	}
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM managed_backup_bundles`).Scan(&count); err != nil || count != 0 {
		t.Fatal("replacement was adopted", count, err)
	}
	if !reflect.DeepEqual(original, restoreTree(t, parked)) || !reflect.DeepEqual(map[string]string{"foreign-note": "preserve"}, restoreTree(t, published.Path)) {
		t.Fatal("registration failure changed original or foreign directory")
	}
}

func TestManagedRegistrationRecoversOnlyExactCommittedOwnership(t *testing.T) {
	for _, change := range []string{"exact", "hash", "destination", "source", "state", "success"} {
		t.Run(change, func(t *testing.T) {
			f, m := newManagedFixture(t, 1)
			published, err := createManaged(t.Context(), f.store, f.dir, m.config.Destination)
			if err != nil {
				t.Fatal(err)
			}
			started := managedStamp(time.Now())
			if err := m.registerCreated(t.Context(), published, started); err != nil {
				t.Fatal("initial actual registration", err)
			}
			switch change {
			case "hash", "destination", "source", "state":
				column, value := "manifest_sha256", strings.Repeat("a", 64)
				switch change {
				case "destination":
					column, value = "destination", filepath.Join(t.TempDir(), "other")
				case "source":
					column, value = "source_instance_id", "instance_"+strings.Repeat("a", 32)
				case "state":
					column, value = "state", "cleanup_failed"
				}
				if _, err := f.store.DB().Exec(`UPDATE managed_backup_bundles SET `+column+`=? WHERE id=?`, value, published.ID); err != nil {
					t.Fatal(err)
				}
			case "success":
				if err := m.update(t.Context(), func(r *managedRecord, _ *sql.Tx) error {
					changed := *r.Status.LastSuccess
					changed.DatabaseBytes++
					r.Status.LastSuccess = &changed
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			rows := backupRows(t, f.store.DB())
			files := restoreTree(t, published.Path)
			err = m.registerCreated(t.Context(), published, started)
			if (err == nil) != (change == "exact") {
				t.Fatal("ownership recovery ignored exact retained identity", change, err)
			}
			if !reflect.DeepEqual(rows, backupRows(t, f.store.DB())) || !reflect.DeepEqual(files, restoreTree(t, published.Path)) {
				t.Fatal("duplicate/rejected registration rewrote retained evidence")
			}
		})
	}
}

func TestManagedRegistrationMultipleDestinationHoldsSurviveRecovery(t *testing.T) {
	f, first := newManagedFixture(t, 1)
	failRegistration := func(m *Manager) createdBundle {
		t.Helper()
		create := m.create
		var published createdBundle
		var removeTrigger func()
		creates := 0
		m.create = func(ctx context.Context, s *core.Store, source, destination string) (createdBundle, error) {
			creates++
			result, err := create(ctx, s, source, destination)
			if err == nil {
				published = result
				removeTrigger = managedRegistrationRejectInserts(t, m)
			}
			return result, err
		}
		reports := runManaged(t, m, 2)
		if removeTrigger != nil {
			removeTrigger()
		}
		m.create = create
		if creates != 1 || len(reports) != 1 || !m.publicationHeld {
			t.Fatal("registration fixture did not hold exactly one publication", creates, reports)
		}
		return published
	}
	one := failRegistration(first)
	cfg := first.config
	cfg.Destination = filepath.Join(t.TempDir(), "second-destination")
	second, err := NewManager(f.store, f.dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	two := failRegistration(second)
	const holdQuery = `SELECT bundle_id,destination,source_instance_id,manifest_sha256,summary_json FROM managed_backup_publication_holds ORDER BY bundle_id`
	holds := restoreRows(t, f.store.DB(), holdQuery)
	if len(holds) != 2 {
		t.Fatal("second failure discarded first publication hold", holds)
	}
	oldFiles := map[string]map[string]string{one.Path: restoreTree(t, one.Path), two.Path: restoreTree(t, two.Path)}
	for _, cfg := range []ScheduleConfig{first.config, second.config, first.config} {
		restarted, err := NewManager(f.store, f.dir, cfg)
		if err != nil || !restarted.publicationHeld {
			t.Fatal("returning to a previously held destination lost its hold", err)
		}
		restarted.create = func(context.Context, *core.Store, string, string) (createdBundle, error) {
			t.Fatal("previously held destination made another copy")
			return createdBundle{}, nil
		}
		if reports := runManaged(t, restarted, 2); len(reports) != 0 {
			t.Fatal(reports)
		}
		status, err := ReadStatus(t.Context(), f.dir)
		if err != nil || status.State != "held" || status.UnregisteredBundle == nil || filepath.Dir(status.UnregisteredBundle.Path) != cfg.Destination || status.LastSuccess != nil {
			t.Fatal("selected destination's hold is not visible", status, err)
		}
	}
	manual, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "recovery-bundle"))
	if err != nil {
		t.Fatal("backup refused retained publication holds", err)
	}
	if _, err := Inspect(t.Context(), manual.Path); err != nil {
		t.Fatal("publication-hold bundle inspection", err)
	}
	dir := filepath.Join(t.TempDir(), "inactive-copy")
	restored, err := Restore(t.Context(), manual.Path, dir)
	if err != nil {
		t.Fatal("restore lost publication-hold schema", err)
	}
	if !reflect.DeepEqual(holds, generationRead(t, dir, holdQuery)) {
		t.Fatal("inactive restore rewrote predecessor publication holds")
	}
	s := activationOpen(t, dir)
	_, err = NewManager(s, dir, first.config)
	backupCode(t, err, "restore_inactive")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, tokenFile := generationToken(t)
	if _, err := Activate(t.Context(), dir, ActivationRequest{ExpectedInstanceID: restored.InstanceID, FencedPredecessorID: restored.SourceInstanceID, AdminTokenFile: tokenFile}); err != nil {
		t.Fatal(err)
	}
	s = activationOpen(t, dir)
	current, err := NewManager(s, dir, first.config)
	if err != nil || current.publicationHeld {
		t.Fatal("predecessor's publication hold became current ownership", err)
	}
	if reports := runManaged(t, current, 1); len(reports) != 0 {
		t.Fatal("fresh generation could not create its own backup", reports)
	}
	if !reflect.DeepEqual(holds, restoreRows(t, s.DB(), holdQuery)) {
		t.Fatal("new generation changed predecessor hold evidence")
	}
	for path, files := range oldFiles {
		if !reflect.DeepEqual(files, restoreTree(t, path)) {
			t.Fatal("recovery adopted or deleted a predecessor's unregistered publication", path)
		}
	}
	var own, predecessor int
	if err := s.DB().QueryRow(`SELECT count(*) FROM managed_backup_bundles WHERE source_instance_id=?`, s.InstanceID()).Scan(&own); err != nil || own != 1 {
		t.Fatal("new generation did not own its actual success", own, err)
	}
	if err := s.DB().QueryRow(`SELECT count(*) FROM managed_backup_bundles WHERE source_instance_id=?`, f.store.InstanceID()).Scan(&predecessor); err != nil || predecessor != 0 {
		t.Fatal("recovery converted predecessor holds into retained ownership", predecessor, err)
	}
}

func TestManagedRegistrationRuntimeHoldWaitsForCancellation(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	removeTrigger := managedRegistrationRejectInserts(t, m)
	defer removeTrigger()
	m.wait = func(context.Context, time.Duration) error { return nil }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	reported := make(chan error, 1)
	go func() { done <- m.Run(ctx, func(err error) { reported <- err }) }()
	select {
	case <-reported:
	case <-time.After(5 * time.Second):
		t.Fatal("registration hold was not reported")
	}
	// Coordination remains usable while the enabled supervisor is idle.
	f.request(t, "POST", "/api/v1/projects/"+f.project+"/messages", map[string]any{"recipient_worker_id": f.worker, "body": "Coordination during publication hold"})
	select {
	case err := <-done:
		t.Fatal("held runtime worker exited before cancellation", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("held worker failed to join cancellation")
	}
}
