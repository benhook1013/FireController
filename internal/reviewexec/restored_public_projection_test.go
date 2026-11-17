package reviewexec

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/presentationmodel"
	"github.com/benhook1013/FireController/internal/publicsite"
	"github.com/benhook1013/FireController/internal/reviews"
)

// A contained projection failure can leave a valid backup behind current review
// facts. Offline reconciliation must retain it without blocking private work.
func TestRestoredPublicProjectionContainsOfflineRefreshAndRecoversOnWrite(t *testing.T) {
	f := attemptProjectionFixture(t, true)
	if err := publicsite.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "public-probe", "cli", 2, 1))
	site := publicsite.New(f.store, reviews.PublicSiteReviewFacts)
	scope := f.scope(t, attemptProject, f.admin, true)
	manifest, err := publicsite.NormalizeManifest(publicsite.Manifest{Slug: "restore-probe", Presentation: presentationmodel.Config{Name: "Restore probe"}, Reviews: []publicsite.EnrolledReview{{Number: 2, Title: "Approved title", URL: "https://github.com/synthetic/project/pull/2"}}})
	if err != nil {
		t.Fatal(err)
	}
	input := publicsite.Approved{ExpectedRevision: 0, Enabled: true, Manifest: manifest}
	preview, err := site.Preview(t.Context(), scope, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = site.Apply(t.Context(), scope, "public-probe-publish", input, preview.SHA256); err != nil {
		t.Fatal(err)
	}
	before, err := site.Current(t.Context(), "restore-probe")
	if err != nil || before.Content.Reviews[0].Lifecycle != "open" {
		t.Fatal("initial public lifecycle", err)
	}
	// Simulate a transient failure in the optional public snapshot storage. The
	// actual observation transaction must commit its authoritative merged state.
	if _, err = f.store.DB().Exec(`CREATE TRIGGER public_probe_transient BEFORE INSERT ON public_project_snapshots BEGIN SELECT RAISE(FAIL,'synthetic transient public storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err = f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, attemptProject).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	pull := projectionPull(2)
	pull.State = "merged"
	first := projectionPull(1)
	first.State = "merged"
	request := reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 801, Source: "imported-history", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{first, pull, projectionPull(4)}}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "public-probe-merged", encoded(request))
	if w.Code != 201 {
		t.Fatal("authoritative observation", w.Code, w.Body.String())
	}
	if _, err = f.store.DB().Exec(`DROP TRIGGER public_probe_transient`); err != nil {
		t.Fatal(err)
	}
	if got := restoredSQL(t, f, `SELECT refresh_error FROM project_publications WHERE project_id=?`, attemptProject); got != "projection_unavailable" {
		t.Fatal("missing contained failure", got)
	}
	if got := restoredSQL(t, f, `SELECT p.state FROM review_workspace w JOIN review_pulls p ON p.project_id=w.project_id AND p.snapshot_id=w.snapshot_id WHERE w.project_id=? AND p.number=2`, attemptProject); got != "merged" {
		t.Fatal("private observation did not commit", got)
	}
	stale, err := site.Current(t.Context(), "restore-probe")
	if err != nil || stale.Cursor != before.Cursor || stale.Content.Reviews[0].Lifecycle != "open" {
		t.Fatal("last-good snapshot was not retained", err)
	}
	read, err := f.store.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	err = publicsite.ValidateSnapshot(t.Context(), read)
	read.Rollback()
	if err != nil {
		t.Fatal("backup public-site validation rejected genuine last-good state", err)
	}
	hints, release, err := f.store.SubscribePublicProjection(attemptProject, "restore-public-test")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	restoredHeld(t, f)
	counts, err := restoredCall(t, f, time.Now())
	if err != nil {
		t.Fatal("offline reconciliation must preserve authoritative attempt cancellation while recovering valid stale projection", err)
	}
	if counts.Attempts != 1 {
		t.Fatal("wrong reconciliation count", counts)
	}
	_, projected := projectedAttempt(t, f, a)
	if projected.State != "cancelled" {
		t.Fatal("attempt not cancelled", projected.State)
	}
	after, err := site.Current(t.Context(), "restore-probe")
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatal("offline reconciliation changed last-good public snapshot", err)
	}
	if got := restoredSQL(t, f, `SELECT refresh_error FROM project_publications WHERE project_id=?`, attemptProject); got != "projection_unavailable" {
		t.Fatal("offline failure not retained", got)
	}
	if got := restoredSQL(t, f, `SELECT p.state FROM review_workspace w JOIN review_pulls p ON p.project_id=w.project_id AND p.snapshot_id=w.snapshot_id WHERE w.project_id=? AND p.number=2`, attemptProject); got != "merged" {
		t.Fatal("offline reconciliation changed authoritative observation", got)
	}
	read, err = f.store.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	err = publicsite.ValidateSnapshot(t.Context(), read)
	read.Rollback()
	if err != nil {
		t.Fatal("offline containment invalidated public snapshot", err)
	}
	select {
	case <-hints:
		t.Fatal("contained offline refresh emitted notification")
	default:
	}
	if err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if err := publicsite.Refresh(ctx, tx, attemptProject, reviews.PublicSiteReviewFacts); err != nil {
			return err
		}
		select {
		case <-hints:
			t.Fatal("public notification emitted before commit")
		default:
		}
		return nil
	}); err != nil {
		t.Fatal("ordinary refresh did not recover", err)
	}
	recovered, err := site.Current(t.Context(), "restore-probe")
	if err != nil || recovered.Generation != before.Generation || recovered.Revision != before.Revision+1 || recovered.Content.Reviews[0].Lifecycle != "merged" || len(recovered.Content.Reviews[0].CLI) != 0 || len(recovered.Content.Reviews[0].Hosted) != 0 {
		t.Fatal("ordinary refresh did not publish real merged state exactly once", err)
	}
	if got := restoredSQL(t, f, `SELECT refresh_error FROM project_publications WHERE project_id=?`, attemptProject); got != "" {
		t.Fatal("successful refresh retained error", got)
	}
	select {
	case <-hints:
	default:
		t.Fatal("recovered public projection missing committed notification")
	}
	select {
	case <-hints:
		t.Fatal("recovered public projection emitted duplicate notification")
	default:
	}
}
