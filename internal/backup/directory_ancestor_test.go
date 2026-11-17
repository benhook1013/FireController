package backup

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func backupDirectoryAlias(t *testing.T, target string) string {
	t.Helper()
	alias := filepath.Join(t.TempDir(), "existing-ancestor")
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("directory symlink fixture unavailable: %v", err)
	}
	return alias
}

func TestBackupDirectoryAncestorsCreateInspectAndInactiveRestore(t *testing.T) {
	f := newRestoreFixture(t)
	alias := backupDirectoryAlias(t, filepath.Dir(f.dir))
	source := filepath.Join(alias, filepath.Base(f.dir))
	queries := map[string]string{"jobs": `SELECT * FROM jobs ORDER BY project_id,id`, "messages": `SELECT * FROM inbox_messages ORDER BY sequence`, "events": `SELECT * FROM events ORDER BY sequence`, "receipts": `SELECT * FROM receipts ORDER BY instance_id,principal_id,project_id,operation,target,key`}
	before := map[string][][]any{}
	for name, query := range queries {
		before[name] = restoreRows(t, f.store.DB(), query)
	}
	captures := restoreTree(t, filepath.Join(f.dir, "review-captures"))
	destination := filepath.Join(alias, "created-parent", "private-bundles")
	bundle, err := Create(t.Context(), f.store, source, destination)
	if err != nil {
		t.Fatal("create through existing higher ancestor", err)
	}
	if bundle.Path != filepath.Join(destination, bundle.ID) {
		t.Fatal("backup changed selected display/IO path")
	}
	for _, path := range []string{filepath.Dir(destination), destination, bundle.Path} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
			t.Fatal("newly created directory is not real/private", path, err)
		}
	}
	bundleBefore := restoreTree(t, bundle.Path)
	if _, err = Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("inspect through existing higher ancestor", err)
	}
	if _, err = CreateFromDirectory(t.Context(), source, destination); err != nil {
		t.Fatal("read-only operator create through higher ancestor", err)
	}
	target := filepath.Join(alias, "inactive-restore")
	restored, err := Restore(t.Context(), bundle.Path, target)
	if err != nil {
		t.Fatal("inactive restore through existing symlink parent", err)
	}
	if restored.State != "inactive" || restored.InstanceID == f.store.InstanceID() || restored.Destination != target {
		t.Fatal("restore identity/state/display path", restored)
	}
	info, err := os.Lstat(target)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		t.Fatal("restored destination is not real/private", err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(target, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for name, query := range queries {
		if !reflect.DeepEqual(before[name], restoreRows(t, f.store.DB(), query)) {
			t.Fatal("backup/inspect/restore changed live source", name)
		}
		if !reflect.DeepEqual(before[name], restoreRows(t, db, query)) {
			t.Fatal("restore lost source data", name)
		}
	}
	if !reflect.DeepEqual(captures, restoreTree(t, filepath.Join(f.dir, "review-captures"))) || !reflect.DeepEqual(bundleBefore, restoreTree(t, bundle.Path)) {
		t.Fatal("source captures or immutable selected bundle changed")
	}
}

func TestBackupDirectoryAncestorsPreserveSelectedAndUnresolvedPathRefusals(t *testing.T) {
	f := newRestoreFixture(t)
	bundle := restoreBundle(t, f)
	before := restoreTree(t, f.dir)
	bundleBefore := restoreTree(t, bundle.Path)
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "selected-link")
	if err := os.Symlink(real, symlink); err != nil {
		t.Skipf("directory symlink fixture unavailable: %v", err)
	}
	dangling := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(root, "absent"), dangling); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("foreign sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{symlink, dangling, file} {
		for _, create := range []bool{false, true} {
			if err := privateDirectory(path, create); err == nil {
				t.Fatal("selected non-real private directory admitted", path, create)
			}
		}
		if _, err := Create(t.Context(), f.store, f.dir, path); err == nil {
			t.Fatal("unsafe selected backup root admitted", path)
		}
		if _, err := Restore(t.Context(), bundle.Path, path); err == nil {
			t.Fatal("existing/symlink destination overlaid", path)
		}
	}
	sourceAlias := filepath.Join(root, "selected-source")
	if err := os.Symlink(f.dir, sourceAlias); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(t.Context(), f.store, sourceAlias, filepath.Join(root, "not-created-source")); err == nil {
		t.Fatal("selected source symlink admitted")
	}
	inspectRoot := filepath.Join(root, "inspect")
	if err := os.Mkdir(inspectRoot, 0700); err != nil {
		t.Fatal(err)
	}
	selectedBundle := filepath.Join(inspectRoot, bundle.ID)
	if err := os.Symlink(bundle.Path, selectedBundle); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(t.Context(), selectedBundle); err == nil {
		t.Fatal("selected immutable bundle symlink admitted")
	}
	for _, ancestor := range []string{dangling, file} {
		child := filepath.Join(ancestor, "must-not-create")
		if err := privateDirectory(child, true); err == nil {
			t.Fatal("dangling/nondirectory ancestor admitted", ancestor)
		}
		if err := restoreParent(ancestor); err == nil {
			t.Fatal("unresolved restore ancestor admitted", ancestor)
		}
		if _, err := Create(t.Context(), f.store, f.dir, child); err == nil {
			t.Fatal("create through unresolved ancestor admitted", ancestor)
		}
		if _, err := Restore(t.Context(), bundle.Path, child); err == nil {
			t.Fatal("restore through unresolved ancestor admitted", ancestor)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "not-created-source")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused source created destination")
	}
	if body, err := os.ReadFile(file); err != nil || string(body) != "foreign sentinel" {
		t.Fatal("nondirectory sentinel modified", err)
	}
	if info, err := os.Lstat(symlink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("selected alias modified", err)
	}
	if info, err := os.Lstat(dangling); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("dangling alias modified", err)
	}
	if !reflect.DeepEqual(before, restoreTree(t, f.dir)) || !reflect.DeepEqual(bundleBefore, restoreTree(t, bundle.Path)) {
		t.Fatal("refused path operation changed source/bundle bytes")
	}
}

func TestBackupDirectoryAncestorsRefusePhysicalContainmentBeforeWrites(t *testing.T) {
	f := newRestoreFixture(t)
	bundle := restoreBundle(t, f)
	sourceBefore := restoreTree(t, f.dir)
	bundleBefore := restoreTree(t, bundle.Path)
	sourceAlias := backupDirectoryAlias(t, f.dir)
	destination := filepath.Join(sourceAlias, "new-parent", "must-not-create")
	if _, err := Create(t.Context(), f.store, f.dir, destination); err == nil {
		t.Fatal("alias hid backup destination inside live source")
	}
	if _, err := os.Lstat(filepath.Join(f.dir, "new-parent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("containment refusal created source directories")
	}
	bundleAlias := backupDirectoryAlias(t, bundle.Path)
	target := filepath.Join(bundleAlias, "must-not-create")
	if _, err := Restore(t.Context(), bundle.Path, target); err == nil {
		t.Fatal("alias hid restore destination inside immutable bundle")
	}
	if _, err := os.Lstat(filepath.Join(bundle.Path, "must-not-create")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("containment refusal modified bundle")
	}
	if !reflect.DeepEqual(sourceBefore, restoreTree(t, f.dir)) || !reflect.DeepEqual(bundleBefore, restoreTree(t, bundle.Path)) {
		t.Fatal("physical containment refusal changed source/bundle bytes")
	}
}
