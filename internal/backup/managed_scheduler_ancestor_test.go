package backup

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestManagedScheduleDirectoryAncestorsCreateVerifiedBackup(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-selected-directory", true: "existing-selected-directory"}[existing], func(t *testing.T) {
			f := newRestoreFixture(t)
			real := t.TempDir()
			alias := backupDirectoryAlias(t, real)
			destination := filepath.Join(alias, "new-parent", "private-bundles")
			if existing {
				if err := os.MkdirAll(destination, 0700); err != nil {
					t.Fatal(err)
				}
			}
			cfg := ScheduleConfig{Destination: destination, Interval: time.Minute, Keep: 1}
			before := restoreTree(t, real)
			validated, err := ValidateScheduleConfig(f.dir, cfg)
			if err != nil {
				t.Fatal("higher directory alias rejected by schedule validation", err)
			}
			if validated.Destination != destination || !reflect.DeepEqual(before, restoreTree(t, real)) {
				t.Fatal("validation changed destination path or filesystem")
			}
			m, err := NewManager(f.store, f.dir, cfg)
			if err != nil {
				t.Fatal("manager rejected validated ancestor alias", err)
			}
			if reports := runManaged(t, m, 1); len(reports) != 0 {
				t.Fatal("scheduled creation through ancestor alias failed", reports)
			}
			status, err := ReadStatus(t.Context(), f.dir)
			if err != nil || status.LastSuccess == nil || status.LastAttempt == nil || status.LastAttempt.State != "complete" || status.Retention == nil || status.Retention.State != "complete" {
				t.Fatal("scheduled backup did not settle successfully", status, err)
			}
			if status.LastSuccess.Path != filepath.Join(destination, status.LastSuccess.ID) {
				t.Fatal("manager changed selected display/IO path", status.LastSuccess)
			}
			if _, err = Inspect(t.Context(), status.LastSuccess.Path); err != nil {
				t.Fatal("managed backup is not a verified usable bundle", err)
			}
			var retained int
			if err = f.store.DB().QueryRow(`SELECT count(*) FROM managed_backup_bundles WHERE state='retained'`).Scan(&retained); err != nil || retained != 1 {
				t.Fatal("managed backup was not registered exactly once", retained, err)
			}
			info, err := os.Lstat(destination)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				t.Fatal("selected destination did not remain a real directory", err)
			}
		})
	}
}

func TestManagedScheduleDirectoryAncestorsRejectUnsafePaths(t *testing.T) {
	f := newRestoreFixture(t)
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	selected := backupDirectoryAlias(t, real)
	dangling := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(root, "absent"), dangling); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("foreign sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	fileAlias := filepath.Join(root, "file-alias")
	if err := os.Symlink(file, fileAlias); err != nil {
		t.Fatal(err)
	}
	before := restoreTree(t, f.dir)
	for _, destination := range []string{selected, dangling, file, filepath.Join(dangling, "child"), filepath.Join(file, "child"), filepath.Join(fileAlias, "child")} {
		cfg := ScheduleConfig{Destination: destination, Interval: time.Minute, Keep: 1}
		if _, err := ValidateScheduleConfig("", cfg); err == nil {
			t.Fatal("standalone inspection admitted unsafe selected/higher component", destination)
		}
		if _, err := NewManager(f.store, f.dir, cfg); err == nil {
			t.Fatal("manager admitted unsafe selected/higher component", destination)
		}
	}
	if body, err := os.ReadFile(file); err != nil || string(body) != "foreign sentinel" {
		t.Fatal("unsafe path refusal modified foreign file", err)
	}
	if entries, err := os.ReadDir(real); err != nil || len(entries) != 0 {
		t.Fatal("selected symlink refusal wrote into foreign directory", err)
	}
	if !reflect.DeepEqual(before, restoreTree(t, f.dir)) {
		t.Fatal("unsafe schedule refusal changed source files")
	}
}

func TestManagedScheduleDirectoryAncestorsRefusePhysicalOverlap(t *testing.T) {
	f := newRestoreFixture(t)
	alias := backupDirectoryAlias(t, f.dir)
	parentAlias := backupDirectoryAlias(t, filepath.Dir(f.dir))
	grandparentAlias := backupDirectoryAlias(t, filepath.Dir(filepath.Dir(f.dir)))
	before := restoreTree(t, f.dir)
	for _, destination := range []string{filepath.Join(alias, "new-parent", "must-not-create"), filepath.Join(parentAlias, filepath.Base(f.dir)), filepath.Join(grandparentAlias, filepath.Base(filepath.Dir(f.dir)))} {
		cfg := ScheduleConfig{Destination: destination, Interval: time.Minute, Keep: 1}
		if _, err := ValidateScheduleConfig(f.dir, cfg); err == nil {
			t.Fatal("schedule alias bypassed physical source/destination separation", destination)
		}
		if _, err := NewManager(f.store, f.dir, cfg); err == nil {
			t.Fatal("manager alias bypassed physical source/destination separation", destination)
		}
	}
	if _, err := os.Lstat(filepath.Join(f.dir, "new-parent")); !os.IsNotExist(err) {
		t.Fatal("overlap refusal created directories in source", err)
	}
	if !reflect.DeepEqual(before, restoreTree(t, f.dir)) {
		t.Fatal("overlap refusal changed source files")
	}
}
