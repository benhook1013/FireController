package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestManagedRetentionNeverDeletesUnownedForeignOrTampered(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	manual, err := Create(t.Context(), f.store, f.dir, m.config.Destination)
	if err != nil {
		t.Fatal(err)
	}
	foreign := newRestoreFixture(t)
	other, err := Create(t.Context(), foreign.store, foreign.dir, m.config.Destination)
	if err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(m.config.Destination, "operator-notes")
	if err = os.Mkdir(unknown, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{manual.Path, other.Path, unknown} {
		if err = os.WriteFile(filepath.Join(path, "unowned-note"), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	unowned := map[string]map[string]string{}
	for _, path := range []string{manual.Path, other.Path, unknown} {
		unowned[path] = restoreTree(t, path)
	}
	if reports := runManaged(t, m, 1); len(reports) != 0 {
		t.Fatal(reports)
	}
	first, err := ReadStatus(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(first.LastSuccess.Path, "manifest.json")
	original, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(manifestPath, append(original, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	removals := 0
	m.remove = func(context.Context, string, Manifest) error { removals++; return nil }
	reports := runManaged(t, m, 1)
	if len(reports) != 1 {
		t.Fatal("tampered owned bundle did not report once", reports)
	}
	backupCode(t, reports[0], "backup_retention_failed")
	second, err := ReadStatus(t.Context(), f.dir)
	if err != nil || second.LastAttempt.State != "complete" || second.LastSuccess.ID == first.LastSuccess.ID || second.Retention.State != "failed" || removals != 0 {
		t.Fatal("creation and retention outcomes conflated", second, err, removals)
	}
	if _, err = Inspect(t.Context(), second.LastSuccess.Path); err != nil {
		t.Fatal("newest verified bundle lost", err)
	}
	// Repairing bytes does not silently authorize retry of a suspicious cleanup.
	if err = os.WriteFile(manifestPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	if reports = runManaged(t, m, 1); len(reports) != 1 || removals != 0 {
		t.Fatal("cleanup failure was blindly retried", reports, removals)
	}
	for path, tree := range unowned {
		if !reflect.DeepEqual(tree, restoreTree(t, path)) {
			t.Fatal("retention touched an unowned directory")
		}
	}
	var state string
	if err = f.store.DB().QueryRow(`SELECT state FROM managed_backup_bundles WHERE id=?`, first.LastSuccess.ID).Scan(&state); err != nil || state != "cleanup_failed" {
		t.Fatal("suspicious cleanup not durable", state, err)
	}
}
func TestManagedRetentionPreservesPartialCleanupAndDoesNotRetry(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	if reports := runManaged(t, m, 1); len(reports) != 0 {
		t.Fatal(reports)
	}
	first, err := ReadStatus(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	removes := 0
	m.remove = func(_ context.Context, path string, _ Manifest) error {
		removes++
		if err := os.Remove(filepath.Join(path, "manifest.sha256")); err != nil {
			return err
		}
		return errors.New("controlled cleanup stopped after first file")
	}
	for i := 0; i < 2; i++ {
		reports := runManaged(t, m, 1)
		if len(reports) != 1 {
			t.Fatal(reports)
		}
		backupCode(t, reports[0], "backup_retention_failed")
	}
	if removes != 1 {
		t.Fatal("partial directory was retried", removes)
	}
	if _, err = os.Stat(filepath.Join(first.LastSuccess.Path, "firecontroller.sqlite")); err != nil {
		t.Fatal("partial remnants not preserved", err)
	}
	if _, err = os.Stat(filepath.Join(first.LastSuccess.Path, "manifest.sha256")); !os.IsNotExist(err) {
		t.Fatal("controlled partial deletion did not happen")
	}
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil || status.LastSuccess.ID == first.LastSuccess.ID || status.Retention.State != "failed" {
		t.Fatal(status, err)
	}
	if _, err = Inspect(t.Context(), status.LastSuccess.Path); err != nil {
		t.Fatal(err)
	}
	// Recovery of a process interrupted after durable deletion admission leaves
	// the same explicit hold and never grants a new removal opportunity.
	if _, err = f.store.DB().Exec(`UPDATE managed_backup_bundles SET state='deleting' WHERE id=?`, first.LastSuccess.ID); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewManager(f.store, f.dir, m.config)
	if err != nil {
		t.Fatal(err)
	}
	restarted.remove = func(context.Context, string, Manifest) error { t.Fatal("restart retried partial cleanup"); return nil }
	if reports := runManaged(t, restarted, 1); len(reports) != 1 {
		t.Fatal(reports)
	}
}
func TestManagedRetentionFailureDoesNotMaskCreationFailure(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	if reports := runManaged(t, m, 1); len(reports) != 0 {
		t.Fatal(reports)
	}
	before, err := ReadStatus(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	m.create = func(context.Context, *core.Store, string, string) (createdBundle, error) {
		return createdBundle{}, invalid()
	}
	m.remove = func(context.Context, string, Manifest) error { t.Fatal("failed create began retention"); return nil }
	reports := runManaged(t, m, 1)
	if len(reports) != 1 {
		t.Fatal(reports)
	}
	backupCode(t, reports[0], "backup_invalid")
	after, err := ReadStatus(t.Context(), f.dir)
	if err != nil || !reflect.DeepEqual(before.Retention, after.Retention) || before.LastSuccess.ID != after.LastSuccess.ID {
		t.Fatal("creation failure fabricated retention", after, err)
	}
}

func TestManagedRetentionLeavesUnexpectedAdditionAndCancelledInventory(t *testing.T) {
	for _, kind := range []string{"unexpected addition", "cancel before removal"} {
		t.Run(kind, func(t *testing.T) {
			f, m := newManagedFixture(t, 1)
			if reports := runManaged(t, m, 1); len(reports) != 0 {
				t.Fatal(reports)
			}
			first, err := ReadStatus(t.Context(), f.dir)
			if err != nil {
				t.Fatal(err)
			}
			m.remove = func(ctx context.Context, path string, manifest Manifest) error {
				if kind == "unexpected addition" {
					if err := os.WriteFile(filepath.Join(path, "new-unrecognized-file"), []byte("preserve"), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					cancelled, cancel := context.WithCancel(ctx)
					cancel()
					ctx = cancelled
				}
				return removeManagedBundle(ctx, path, manifest)
			}
			if reports := runManaged(t, m, 1); len(reports) != 1 {
				t.Fatal(reports)
			}
			if kind == "unexpected addition" {
				body, err := os.ReadFile(filepath.Join(first.LastSuccess.Path, "new-unrecognized-file"))
				if err != nil || string(body) != "preserve" {
					t.Fatal("retention recursively deleted an unknown addition", err)
				}
			} else if _, err := Inspect(t.Context(), first.LastSuccess.Path); err != nil {
				t.Fatal("pre-removal cancellation changed the bundle", err)
			}
			status, err := ReadStatus(t.Context(), f.dir)
			if err != nil || status.Retention.State != "failed" || status.LastSuccess.ID == first.LastSuccess.ID {
				t.Fatal("cleanup failure lost the new success", status, err)
			}
		})
	}
}

func TestManagedRetentionVerifiesAllKeepersBeforePruning(t *testing.T) {
	for _, kind := range []string{"tampered keeper", "missing keeper", "foreign keeper", "all verified"} {
		t.Run(kind, func(t *testing.T) {
			f, m := newManagedFixture(t, 2)
			if reports := runManaged(t, m, 1); len(reports) != 0 {
				t.Fatal(reports)
			}
			first, err := ReadStatus(t.Context(), f.dir)
			if err != nil {
				t.Fatal(err)
			}
			firstBytes := restoreTree(t, first.LastSuccess.Path)
			if reports := runManaged(t, m, 1); len(reports) != 0 {
				t.Fatal(reports)
			}
			second, err := ReadStatus(t.Context(), f.dir)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "tampered keeper":
				file := filepath.Join(second.LastSuccess.Path, "manifest.json")
				body, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, append(body, ' '), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing keeper":
				if err := os.RemoveAll(second.LastSuccess.Path); err != nil {
					t.Fatal(err)
				}
			case "foreign keeper":
				foreign := newRestoreFixture(t)
				other, err := Create(t.Context(), foreign.store, foreign.dir, filepath.Join(t.TempDir(), "foreign"))
				if err != nil {
					t.Fatal(err)
				}
				resealBundle(t, other.Path, func(manifest *Manifest) { manifest.ID = second.LastSuccess.ID })
				if err := os.RemoveAll(second.LastSuccess.Path); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(other.Path, second.LastSuccess.Path); err != nil {
					t.Fatal(err)
				}
				verified, err := Inspect(t.Context(), second.LastSuccess.Path)
				if err != nil || verified.SourceInstanceID == f.store.InstanceID() {
					t.Fatal("foreign keeper must be independently valid", err)
				}
			}
			reports := runManaged(t, m, 1)
			current, err := ReadStatus(t.Context(), f.dir)
			if err != nil || current.LastAttempt.State != "complete" || current.LastSuccess.ID == second.LastSuccess.ID {
				status, _ := json.Marshal(current)
				t.Fatalf("new success was not retained independently: status=%s previous_success_id=%s reports=%v read_error=%v", status, second.LastSuccess.ID, reports, err)
			}
			if _, err := Inspect(t.Context(), current.LastSuccess.Path); err != nil {
				t.Fatal(err)
			}
			if kind == "all verified" {
				if len(reports) != 0 || current.Retention.State != "complete" {
					t.Fatal(reports, current.Retention)
				}
				if _, err := os.Lstat(first.LastSuccess.Path); !os.IsNotExist(err) {
					t.Fatal("oldest verified excess copy was not pruned")
				}
				if _, err := Inspect(t.Context(), second.LastSuccess.Path); err != nil {
					t.Fatal("keeper was changed", err)
				}
			} else {
				if len(reports) != 1 {
					t.Fatal("bad keeper must stop pruning and report once", reports)
				}
				backupCode(t, reports[0], "backup_retention_failed")
				if current.Retention.State != "failed" || !reflect.DeepEqual(firstBytes, restoreTree(t, first.LastSuccess.Path)) {
					t.Fatal("a bad keeper allowed deletion of a usable older copy")
				}
			}
		})
	}
}
