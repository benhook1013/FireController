package backup

import (
	"bytes"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewruntime"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExitedCLICaptureV3BackupRestore(t *testing.T) {
	f := newRestoreFixture(t)
	store, err := reviewruntime.NewCaptureStore(filepath.Join(f.dir, "review-captures"))
	if err != nil {
		t.Fatal(err)
	}
	id := "review_attempt_" + strings.Repeat("a", 32)
	at := time.Now().UTC()
	ref, err := store.Save(t.Context(), id, reviewruntime.CaptureRecord{Capture: reviewprocess.Capture{Stdout: []byte("retained original terminal output"), Started: true, Exited: true, ExitCode: 0}, StartedAt: at, FinishedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(f.dir, "review-captures", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(original, []byte(`"version":3`)) {
		t.Fatal("not v3", string(original))
	}
	b := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), b.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	if _, err := Restore(t.Context(), b.Path, destination); err != nil {
		t.Fatal(err)
	}
	restored, err := reviewruntime.NewCaptureStore(filepath.Join(destination, "review-captures"))
	if err != nil {
		t.Fatal(err)
	}
	record, got, err := restored.Read(t.Context(), id)
	if err != nil || got != ref || !record.Capture.Exited || !record.Capture.Started || record.Capture.ExitCode != 0 {
		t.Fatal("exit proof lost", record, got, err)
	}
	copied, err := os.ReadFile(filepath.Join(destination, "review-captures", id+".json"))
	if err != nil || !bytes.Equal(original, copied) {
		t.Fatal("raw capture changed", err)
	}
}
