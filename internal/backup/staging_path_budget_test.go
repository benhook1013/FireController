package backup

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func stagingBoundaryDestination(t *testing.T, extra int) string {
	t.Helper()
	// Exercise the real bundled Unix VFS boundary within a disposable private
	// tree. Windows' compiled VFS may exceed ordinary filesystem path bounds.
	if runtime.GOOS == "windows" {
		t.Skip("real Unix VFS pathname boundary")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const stage = ".backup_00000000000000000000000000000000.pending"
	size := backupSQLitePathLimit() - len(filepath.Join("/", stage, "firecontroller.sqlite")) + extra
	if len(root)+2 > size || size > 2048 {
		t.Fatal("unexpected compiled VFS budget", size, root)
	}
	for len(root) < size {
		n := size - len(root) - 1
		if n > 180 {
			n = 180
		}
		if n == 0 {
			root += "x"
		} else {
			root = filepath.Join(root, strings.Repeat("x", n))
		}
	}
	return root
}

func TestBackupStagedPathExposesFinalNameOnlyBudgetRegression(t *testing.T) {
	f := newRestoreFixture(t)
	destination := stagingBoundaryDestination(t, 1)
	const id = "backup_00000000000000000000000000000000"
	final := filepath.Join(destination, id)
	stage := filepath.Join(destination, "."+id+".pending")
	for _, path := range []string{final, stage} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.Snapshot(t.Context(), filepath.Join(final, "firecontroller.sqlite")); err != nil {
		t.Fatal("the final-name database still fits the real VFS", err)
	}
	if err := f.store.Snapshot(t.Context(), filepath.Join(stage, "firecontroller.sqlite")); err == nil {
		t.Fatal("longer staging pathname unexpectedly fit the real VFS")
	}
	if _, err := os.Lstat(filepath.Join(stage, "firecontroller.sqlite")); !os.IsNotExist(err) {
		t.Fatal("failed staged snapshot left a database", err)
	}
	if _, err := ValidateScheduleConfig(f.dir, ScheduleConfig{Destination: destination, Interval: time.Minute, Keep: 1}); err == nil {
		t.Fatal("managed configuration admitted an unusable staged path")
	}
}

func TestBackupStagedPathExactBoundaryCreatesVerifiedManualAndManagedBundles(t *testing.T) {
	for _, mode := range []string{"manual", "directory", "managed"} {
		t.Run(mode, func(t *testing.T) {
			f := newRestoreFixture(t)
			destination := stagingBoundaryDestination(t, 0)
			if _, err := ValidateScheduleConfig(f.dir, ScheduleConfig{Destination: destination, Interval: time.Minute, Keep: 1}); err != nil {
				t.Fatal("exact staged-path budget rejected", err)
			}
			var result Summary
			var err error
			switch mode {
			case "manual":
				result, err = Create(t.Context(), f.store, f.dir, destination)
			case "directory":
				result, err = CreateFromDirectory(t.Context(), f.dir, destination)
			case "managed":
				var m *Manager
				m, err = NewManager(f.store, f.dir, ScheduleConfig{Destination: destination, Interval: time.Minute, Keep: 1})
				if err == nil {
					if reports := runManaged(t, m, 1); len(reports) != 0 {
						t.Fatal(reports)
					}
					status, readErr := ReadStatus(t.Context(), f.dir)
					if readErr != nil || status.LastSuccess == nil {
						t.Fatal(status, readErr)
					}
					result = *status.LastSuccess
				}
			}
			if err != nil {
				t.Fatal("exact staged-path boundary failed creation", err)
			}
			if _, err := Inspect(t.Context(), result.Path); err != nil {
				t.Fatal("created boundary bundle is unusable", err)
			}
		})
	}
}

func TestBackupStagedPathOverflowRefusedBeforeDirectoryCreation(t *testing.T) {
	for _, mode := range []string{"manual", "directory", "managed"} {
		t.Run(mode, func(t *testing.T) {
			f := newRestoreFixture(t)
			destination := stagingBoundaryDestination(t, 1)
			var err error
			switch mode {
			case "manual":
				_, err = Create(t.Context(), f.store, f.dir, destination)
			case "directory":
				_, err = CreateFromDirectory(t.Context(), f.dir, destination)
			case "managed":
				_, err = NewManager(f.store, f.dir, ScheduleConfig{Destination: destination, Interval: time.Minute, Keep: 1})
			}
			backupCode(t, err, "configuration_invalid")
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatal("refused destination was created", err)
			}
		})
	}
}
