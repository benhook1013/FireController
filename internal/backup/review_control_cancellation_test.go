package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestReviewControlValidationCancellationIsUnavailableAndReadOnly(t *testing.T) {
	for _, phase := range []string{"ValidateRoundSnapshot", "ValidateSelectionSnapshot", "ValidateFrontSnapshot"} {
		t.Run(phase, func(t *testing.T) {
			var f *restoreFixture
			if phase == "ValidateRoundSnapshot" {
				f = roundInventoryFixture(t, false)
			} else {
				f, _ = selectionInventoryFixture(t, "held")
			}
			bundle := restoreBundle(t, f)
			rows, files := backupRows(t, f.store.DB()), restoreTree(t, bundle.Path)
			for _, operation := range []string{"inspect", "restore", "create"} {
				t.Run(operation, func(t *testing.T) {
					base, cancel := context.WithCancel(t.Context())
					defer cancel()
					// Reuse the existing SQL-phase cancellation fixture: earlier
					// inventory checks finish normally, without a timer or hook.
					ctx := &historyScanCancellation{Context: base, cancel: cancel, phase: "github.com/benhook1013/FireController/internal/reviewexec." + phase}
					target := filepath.Join(t.TempDir(), "output")
					var err error
					switch operation {
					case "inspect":
						_, err = Inspect(ctx, bundle.Path)
					case "restore":
						_, err = Restore(ctx, bundle.Path, target)
					case "create":
						_, err = Create(ctx, f.store, f.dir, target)
					}
					if !ctx.fired.Load() || !errors.Is(base.Err(), context.Canceled) {
						t.Fatal("selected review validator was not interrupted", err)
					}
					var failure *core.Error
					if !errors.As(err, &failure) || failure.Code != "backup_unavailable" || failure.Status != 503 {
						t.Error("review validation interruption lost retryable classification", err)
					}
					if operation == "create" {
						entries, readErr := os.ReadDir(target)
						if readErr != nil || len(entries) != 0 {
							t.Fatal("interrupted creation retained a partial bundle", readErr)
						}
					} else if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatal("interrupted verification created restore output", statErr)
					}
					if !reflect.DeepEqual(rows, backupRows(t, f.store.DB())) || !reflect.DeepEqual(files, restoreTree(t, bundle.Path)) {
						t.Fatal("interrupted verification changed source rows or bundle")
					}
				})
			}
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal("transient cancellation became lasting corruption", err)
			}
		})
	}
}

func TestReviewControlValidationCorruptionRemainsInvalid(t *testing.T) {
	for _, phase := range []string{"round", "selection", "front"} {
		t.Run(phase, func(t *testing.T) {
			var f *restoreFixture
			query := `UPDATE review_round_controls SET maximum=5`
			if phase == "round" {
				f = roundInventoryFixture(t, false)
			} else {
				f, _ = selectionInventoryFixture(t, "held")
				query = `UPDATE review_selections SET updated_at='unknown'`
				if phase == "front" {
					query = `UPDATE review_front_frames SET prefix_json='null'`
				}
			}
			bundle := restoreBundle(t, f)
			rows := backupRows(t, f.store.DB())
			mutateBundleSQL(t, bundle.Path, query)
			files := restoreTree(t, bundle.Path)
			for _, operation := range []string{"inspect", "restore"} {
				target := filepath.Join(t.TempDir(), "restored")
				var err error
				if operation == "inspect" {
					_, err = Inspect(t.Context(), bundle.Path)
				} else {
					_, err = Restore(t.Context(), bundle.Path, target)
				}
				var failure *core.Error
				if !errors.As(err, &failure) || failure.Code != "backup_invalid" || failure.Status != 400 {
					t.Fatalf("%s changed genuine corruption classification: %v", operation, err)
				}
				if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatal("invalid bundle created restore output", statErr)
				}
			}
			if !reflect.DeepEqual(rows, backupRows(t, f.store.DB())) || !reflect.DeepEqual(files, restoreTree(t, bundle.Path)) {
				t.Fatal("corruption refusal changed source rows or bundle")
			}
		})
	}
}
