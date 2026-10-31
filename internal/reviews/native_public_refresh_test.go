package reviews

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/presentationmodel"
	"github.com/benhook1013/FireController/internal/publicsite"
)

func TestNativePrivateAttemptSkipsPublicBuilderButPublicFactsRefresh(t *testing.T) {
	f := newAPIFixture(t)
	if err := publicsite.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	pull := nativePull(1)
	pull.State = "open"
	f.observe(t, reviewRequest(0, []PullInput{pull}), "initial-public-lifecycle")
	site := publicsite.New(f.store, PublicSiteReviewFacts)
	r := httptest.NewRequest("PUT", "/", nil)
	r.SetPathValue("project_id", f.project)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	scope, err := f.store.AdminScope(r)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := publicsite.NormalizeManifest(publicsite.Manifest{Slug: "native-public-noop", Presentation: presentationmodel.Config{Name: "Native public test"}, Reviews: []publicsite.EnrolledReview{{Number: 1, Title: "First PR", URL: "https://github.com/owner/reviews/pull/1"}, {Number: 2, Title: "Second PR", URL: "https://github.com/owner/reviews/pull/2"}}})
	if err != nil {
		t.Fatal(err)
	}
	approved := publicsite.Approved{ExpectedRevision: 0, Enabled: true, Manifest: manifest}
	preview, err := site.Preview(t.Context(), scope, approved)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := site.Apply(t.Context(), scope, "enable-native-public", approved, preview.SHA256); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{`CREATE TABLE native_public_probe(calls INTEGER NOT NULL)`, `INSERT INTO native_public_probe VALUES(0)`, `CREATE TRIGGER native_public_refresh_probe AFTER UPDATE ON project_publications BEGIN UPDATE native_public_probe SET calls=calls+1; END`} {
		if _, err := f.store.DB().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	read := func() publicsite.Snapshot {
		t.Helper()
		value, err := site.Current(t.Context(), manifest.Slug)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	probe := func() int {
		t.Helper()
		var value int
		if err := f.store.DB().QueryRow(`SELECT calls FROM native_public_probe`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	first := read()
	attempt := nativeAttempt("private-active", "active")
	privateBefore := nativeCurrentRevision(t, f)
	mustNativePublish(t, f, NativePublication{Pull: pull, Attempt: &attempt})
	if nativeCurrentRevision(t, f) <= privateBefore || probe() != 0 || !reflect.DeepEqual(first, read()) {
		t.Fatal("private attempt rebuilt or changed public projection")
	}
	// Qualify the probe against the real full builder, even when it computes
	// identical content: successful no-op cleanup still updates publication.
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return publicsite.Refresh(ctx, tx, f.project, PublicSiteReviewFacts)
	}); err != nil {
		t.Fatal(err)
	}
	if probe() != 1 || !reflect.DeepEqual(first, read()) {
		t.Fatal("probe did not observe the actual unchanged-content builder")
	}
	pull.State = "closed"
	attempt = nativeAttempt("lifecycle-change", "active")
	mustNativePublish(t, f, NativePublication{Pull: pull, Attempt: &attempt})
	closed := read()
	if closed.Revision != first.Revision+1 || closed.Content.Reviews[0].Lifecycle != "closed" || probe() <= 1 {
		t.Fatal("attempt publication hid a lifecycle change", closed)
	}
	newPull := nativePull(2)
	newPull.State = "open"
	attempt = nativeAttempt("initial-second-pr", "active")
	mustNativePublish(t, f, NativePublication{Pull: newPull, Attempt: &attempt})
	introduced := read()
	if introduced.Revision != closed.Revision+1 || introduced.Content.Reviews[1].Lifecycle != "open" {
		t.Fatal("new PR attempt did not publish its initial lifecycle", introduced)
	}
	run := nativeRun("new-public-finalized")
	mustNativePublish(t, f, NativePublication{Pull: pull, Run: &run})
	finalized := read()
	if finalized.Revision != introduced.Revision+1 || len(finalized.Content.Reviews[0].CLI) != 1 {
		t.Fatal("finalized run did not refresh public facts", finalized)
	}
	// Optional malformed public configuration must not turn an unrelated
	// private attempt into projection work or a fresh projection error.
	if _, err := f.store.DB().Exec(`UPDATE project_publications SET manifest_json='{' WHERE project_id=?`, f.project); err != nil {
		t.Fatal(err)
	}
	priorProbe := probe()
	attempt = nativeAttempt("private-after-invalid-public-record", "active")
	mustNativePublish(t, f, NativePublication{Pull: pull, Attempt: &attempt})
	var refreshError string
	if err := f.store.DB().QueryRow(`SELECT refresh_error FROM project_publications WHERE project_id=?`, f.project).Scan(&refreshError); err != nil {
		t.Fatal(err)
	}
	if refreshError != "" || probe() != priorProbe || !reflect.DeepEqual(finalized, read()) {
		t.Fatal("private attempt touched malformed optional public projection", refreshError)
	}
}

func nativeCurrentRevision(t *testing.T, f *apiFixture) int64 {
	t.Helper()
	var revision int64
	if err := f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, f.project).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return revision
}
