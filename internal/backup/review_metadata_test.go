package backup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviews"
)

func TestReviewMetadataCanonicalBackupRestoreAndCorruption(t *testing.T) {
	f := historyInventoryAPIFixture(t)
	draft := false
	revision := int64(0)
	pull := reviews.PullInput{Number: 1, Title: "Metadata roundtrip", State: "merged", HeadSHA: strings.Repeat("a", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40), CIState: "unknown", Mergeability: "unknown", Diff: &reviews.PullDiff{}, IsDraft: &draft, MergedAt: "2026-01-01T00:00:00.123456789Z"}
	f.request(t, http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 991, Source: "metadata-import", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{pull}})
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	after := backupRows(t, db)
	for _, table := range []string{"review_pull_versions", "review_pull_memberships", "review_snapshots", "review_current_pulls"} {
		if after[table] != before[table] {
			t.Fatal("restored metadata/source changed", table)
		}
	}
	if err := reviews.ValidatePullMetadataHistory(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("backup/inspect changed source")
	}
	for _, raw := range []string{`{"diff":{"files":-1,"additions":0,"deletions":0}}`, `{"merged_at":"invalid"}`, `{"unknown":true}`} {
		if _, err := f.store.DB().Exec(`UPDATE review_pull_versions SET metadata_json=?`, raw); err != nil {
			t.Fatal(err)
		}
		if _, err := Create(context.Background(), f.store, f.dir, filepath.Join(t.TempDir(), "refused")); err == nil {
			t.Fatal("corrupt metadata backed up", raw)
		}
	}
}

func TestReviewMetadataReadOnlyHTTPAfterBackup(t *testing.T) {
	f := historyInventoryAPIFixture(t)
	revision := int64(0)
	draft := false
	f.request(t, http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 991, Source: "metadata-import", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{{Number: 1, Title: "Known zero", State: "open", CIState: "unknown", Mergeability: "unknown", Diff: &reviews.PullDiff{}, IsDraft: &draft}}})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/queue", nil)
	request.Header.Set("Authorization", "Bearer "+f.token)
	request.Header.Set("X-FC-Instance", f.store.InstanceID())
	out := httptest.NewRecorder()
	f.handler.ServeHTTP(out, request)
	var response reviews.QueueResponse
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &response) != nil || response.Items[0].Diff == nil || response.Items[0].IsDraft == nil {
		t.Fatal("real HTTP metadata lost", out.Code)
	}
}
