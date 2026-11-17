package backup

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func TestManagedRetentionSkipsVerificationWithoutPruningCandidate(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(map[int]string{1: "below_keep", 2: "at_keep"}[count], func(t *testing.T) {
			f, m := newManagedFixture(t, 2)
			if reports := runManaged(t, m, count); len(reports) != 0 {
				t.Fatal(reports)
			}
			prior, err := ReadStatus(t.Context(), f.dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(prior.LastSuccess.Path, "manifest.json"), []byte("damaged keeper"), 0600); err != nil {
				t.Fatal(err)
			}
			rows, files := backupRows(t, f.store.DB()), restoreTree(t, m.config.Destination)
			m.remove = func(context.Context, string, Manifest) error {
				t.Fatal("no-candidate retention attempted deletion")
				return nil
			}
			if err := m.retain(t.Context()); err != nil {
				t.Fatal("no-candidate retention unnecessarily inspected keeper bytes", err)
			}
			if !reflect.DeepEqual(rows, backupRows(t, f.store.DB())) || !reflect.DeepEqual(files, restoreTree(t, m.config.Destination)) {
				t.Fatal("no-candidate retention changed rows or damaged keeper bytes")
			}
			// The successful no-prune pass grants no present-byte validity.
			if _, err := Inspect(t.Context(), prior.LastSuccess.Path); err == nil {
				t.Fatal("damaged fixture unexpectedly became a usable backup")
			}
		})
	}
}

func TestManagedRetentionPruningStillVerifiesDamagedProtectedKeeper(t *testing.T) {
	f, m := newManagedFixture(t, 2)
	if reports := runManaged(t, m, 1); len(reports) != 0 {
		t.Fatal(reports)
	}
	oldest, err := ReadStatus(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if reports := runManaged(t, m, 1); len(reports) != 0 {
		t.Fatal(reports)
	}
	keeper, err := ReadStatus(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keeper.LastSuccess.Path, "manifest.json"), []byte("damaged keeper"), 0600); err != nil {
		t.Fatal(err)
	}
	oldestFiles, keeperFiles := restoreTree(t, oldest.LastSuccess.Path), restoreTree(t, keeper.LastSuccess.Path)
	if err := m.retain(t.Context()); err != nil {
		t.Fatal("at-keep maintenance performed premature verification", err)
	}
	m.remove = func(context.Context, string, Manifest) error {
		t.Fatal("damaged protected keeper permitted deletion")
		return nil
	}
	reports := runManaged(t, m, 1)
	if len(reports) != 1 {
		t.Fatal("excess retention did not report damaged protected keeper", reports)
	}
	backupCode(t, reports[0], "backup_retention_failed")
	current, err := ReadStatus(t.Context(), f.dir)
	if err != nil || current.LastSuccess == nil || current.LastSuccess.ID == keeper.LastSuccess.ID || current.LastAttempt.State != "complete" || current.Retention == nil || current.Retention.State != "failed" {
		t.Fatal("keeper refusal lost newly registered success", current, err)
	}
	if _, err := Inspect(t.Context(), current.LastSuccess.Path); err != nil {
		t.Fatal("newly registered backup is unusable", err)
	}
	if !reflect.DeepEqual(oldestFiles, restoreTree(t, oldest.LastSuccess.Path)) || !reflect.DeepEqual(keeperFiles, restoreTree(t, keeper.LastSuccess.Path)) {
		t.Fatal("pruning changed protected history after failed verification")
	}
	var retained, deleted int
	if err := f.store.DB().QueryRow(`SELECT sum(state='retained'),sum(state='deleted') FROM managed_backup_bundles`).Scan(&retained, &deleted); err != nil || retained != 3 || deleted != 0 {
		t.Fatal("damaged keeper did not prevent all pruning", retained, deleted, err)
	}
}

func TestManagedRetentionGetsFreshBudgetAfterCompletedCreationExpires(t *testing.T) {
	f, initial := newManagedFixture(t, 1)
	if reports := runManaged(t, initial, 1); len(reports) != 0 {
		t.Fatal(reports)
	}
	previous, err := ReadStatus(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := initial.config
	cfg.Timeout = 5 * time.Second
	m, err := NewManager(f.store, f.dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	create, remove := m.create, m.remove
	var creationDeadline, retentionDeadline time.Time
	m.create = func(ctx context.Context, store *core.Store, source, destination string) (createdBundle, error) {
		result, err := create(ctx, store, source, destination)
		if err != nil {
			return result, err
		}
		var ok bool
		creationDeadline, ok = ctx.Deadline()
		if !ok {
			t.Fatal("creation has no bounded context")
		}
		// Preserve actual completed creation evidence while exhausting only
		// that phase's context before managed registration/retention begins.
		<-ctx.Done()
		return result, nil
	}
	m.remove = func(ctx context.Context, path string, manifest Manifest) error {
		var ok bool
		retentionDeadline, ok = ctx.Deadline()
		parentDeadline, _ := parent.Deadline()
		if !ok || ctx.Err() != nil || !retentionDeadline.After(creationDeadline) || retentionDeadline.After(parentDeadline) || time.Until(retentionDeadline) > cfg.Timeout {
			t.Fatal("retention reused exhausted or unbounded context", retentionDeadline, creationDeadline)
		}
		return remove(ctx, path, manifest)
	}
	if err := m.attempt(parent); err != nil {
		t.Fatal("completed creation consumed retention's phase budget", err)
	}
	if retentionDeadline.IsZero() || parent.Err() != nil {
		t.Fatal("real pruning did not run under the live parent context")
	}
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil || status.LastSuccess == nil || status.LastSuccess.ID == previous.LastSuccess.ID || status.LastAttempt.State != "complete" || status.Retention == nil || status.Retention.State != "complete" {
		t.Fatal("fresh retention budget lost settled success", status, err)
	}
	if _, err := os.Lstat(previous.LastSuccess.Path); !os.IsNotExist(err) {
		t.Fatal("excess verified backup was not pruned", err)
	}
	if _, err := Inspect(t.Context(), status.LastSuccess.Path); err != nil {
		t.Fatal("protected current backup lost validity", err)
	}
}
