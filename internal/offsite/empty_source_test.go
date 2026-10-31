package offsite

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Public Push may validate the configured transport, but an empty source must
// stop before executing it or publishing any pending/archive/remote identity.
func emptySourceConfig(t *testing.T, c Config) string {
	t.Helper()
	if err := os.WriteFile(c.RcloneBinary, []byte("not an executable transport"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.RcloneConfig, []byte("private synthetic configuration"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "offsite.json")
	if err := writeJSON(path, c); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOffsitePublicPushEmptyDirectoryReportsEmptySource(t *testing.T) {
	f := newFixture(t)
	if err := os.Mkdir(f.c.SourceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	config := emptySourceConfig(t, f.c)
	result, err := Push(t.Context(), config)
	wantCode(t, err, "offsite_source_empty")
	if result != (Result{}) {
		t.Fatal("empty source invented a publication", result)
	}
	st := state(t, f.c)
	if st.LastAttempt == nil || st.LastAttempt.State != "failed" || st.LastAttempt.Reason != "offsite_source_empty" || st.LastAttempt.FinishedAt == "" || st.Pending != nil || st.LastSuccess != nil || len(st.Owned) != 0 {
		t.Fatal("empty-source status lost its truthful cause", st)
	}
	for _, name := range []string{"work", "prepare.tar.gz", "pending.tar.gz"} {
		if _, err := os.Lstat(filepath.Join(f.c.StateDir, name)); !os.IsNotExist(err) {
			t.Fatal("empty source left transfer work", name, err)
		}
	}
	entries, err := os.ReadDir(f.c.SourceRoot)
	if err != nil || len(entries) != 0 {
		t.Fatal("source was modified", entries, err)
	}
	// EOF is not a blanket exemption for malformed candidate entries.
	if err := os.WriteFile(filepath.Join(f.c.SourceRoot, "backup_00000000000000000000000000000001"), []byte("not a bundle"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = Push(t.Context(), config)
	wantCode(t, err, "offsite_source_invalid")
}

func TestOffsiteEmptySourcePreservesEarlierVerifiedSuccess(t *testing.T) {
	f := newFixture(t)
	bundle := f.bundle(t)
	remote := memory()
	first, err := push(t.Context(), f.c, remote)
	if err != nil || first.State != "complete" {
		t.Fatal(first, err)
	}
	before := state(t, f.c)
	if before.LastSuccess == nil {
		t.Fatal("fixture did not publish verified success")
	}
	config := emptySourceConfig(t, f.c)
	if err := os.RemoveAll(bundle.Path); err != nil {
		t.Fatal(err)
	}
	result, err := Push(t.Context(), config)
	wantCode(t, err, "offsite_source_empty")
	after := state(t, f.c)
	if result != (Result{}) || after.LastAttempt.State != "failed" || after.LastAttempt.Reason != "offsite_source_empty" || after.Pending != nil || !reflect.DeepEqual(after.LastSuccess, before.LastSuccess) || !reflect.DeepEqual(after.Owned, before.Owned) || after.RetentionReason != before.RetentionReason {
		t.Fatal("empty local source erased prior verified success/ownership", before, after, result)
	}
	report, err := ReadStatus(t.Context(), config)
	if err != nil || report.LastSuccess == nil || report.LastSuccess.ID != bundle.ID || report.LastAttempt.Reason != "offsite_source_empty" {
		t.Fatal("public status conflated attempt failure with verified backup", report, err)
	}
}
