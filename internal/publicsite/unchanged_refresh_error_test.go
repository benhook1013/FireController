package publicsite

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

// A real SQLite failure only on the unchanged-content success cleanup. The
// optional-refresh containment UPDATE must remain able to record its failure.
func rejectUnchangedRefreshClear(t *testing.T, f *publicationFixture) {
	t.Helper()
	if _, err := f.store.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_unchanged_refresh_clear BEFORE UPDATE OF refresh_error ON project_publications WHEN NEW.refresh_error='' BEGIN SELECT RAISE(ABORT,'synthetic refresh cleanup failure'); END`); err != nil {
		t.Fatal(err)
	}
}

func unchangedRefreshCounts(t *testing.T, f *publicationFixture) [3]int64 {
	t.Helper()
	var counts [3]int64
	for i, query := range []string{`SELECT COUNT(*) FROM public_project_snapshots`, `SELECT COUNT(*) FROM receipts`, `SELECT COUNT(*) FROM events`} {
		if err := f.store.DB().QueryRowContext(t.Context(), query).Scan(&counts[i]); err != nil {
			t.Fatal(err)
		}
	}
	return counts
}

func TestUnchangedPublicationRefreshClearFailureIsUnavailable(t *testing.T) {
	f := newPublicationFixture(t)
	if w := f.apply(t, Approved{0, true, testPublicManifest()}, "publish"); w.Code != http.StatusOK {
		t.Fatal("publish", w.Code)
	}
	before := f.public(t)
	counts := unchangedRefreshCounts(t, f)
	if _, err := f.store.DB().ExecContext(t.Context(), `UPDATE project_publications SET refresh_error='projection_unavailable' WHERE project_id=?`, f.project); err != nil {
		t.Fatal(err)
	}
	rejectUnchangedRefreshClear(t, f)
	var branchErr error
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		r, found, err := readRecord(ctx, tx, f.project)
		if err != nil || !found {
			t.Fatal("publication record", err)
		}
		var content string
		if err := tx.QueryRowContext(ctx, `SELECT content_json FROM public_project_snapshots WHERE project_id=? AND generation=? AND revision=?`, r.project, r.generation, r.publicRevision).Scan(&content); err != nil {
			t.Fatal(err)
		}
		branchErr = publishContent(ctx, tx, r, content)
		return branchErr
	})
	var failure *core.Error
	if !errors.As(branchErr, &failure) || failure.Code != "publication_unavailable" || failure.Status != http.StatusServiceUnavailable {
		t.Fatalf("unchanged publication cleanup must return publication_unavailable/503, got %T", branchErr)
	}
	if err == nil {
		t.Fatal("failed publication cleanup committed")
	}
	var code string
	if err := f.store.DB().QueryRowContext(t.Context(), `SELECT refresh_error FROM project_publications WHERE project_id=?`, f.project).Scan(&code); err != nil || code != "projection_unavailable" {
		t.Fatal("failed cleanup changed retained refresh status", err, code)
	}
	if !reflect.DeepEqual(f.public(t), before) || unchangedRefreshCounts(t, f) != counts {
		t.Fatal("failed unchanged cleanup changed public bytes, revision, receipt or event history")
	}

	// Removing the injected fault preserves normal cleanup without publishing a
	// new revision, receipt or public freshness event.
	if _, err := f.store.DB().ExecContext(t.Context(), `DROP TRIGGER reject_unchanged_refresh_clear`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		r, found, err := readRecord(ctx, tx, f.project)
		if err != nil || !found {
			return unavailable()
		}
		var content string
		if err := tx.QueryRowContext(ctx, `SELECT content_json FROM public_project_snapshots WHERE project_id=? AND generation=? AND revision=?`, r.project, r.generation, r.publicRevision).Scan(&content); err != nil {
			return err
		}
		return publishContent(ctx, tx, r, content)
	}); err != nil {
		t.Fatal("unchanged cleanup recovery", err)
	}
	if err := f.store.DB().QueryRowContext(t.Context(), `SELECT refresh_error FROM project_publications WHERE project_id=?`, f.project).Scan(&code); err != nil || code != "" {
		t.Fatal("successful cleanup did not clear refresh status", err, code)
	}
	if !reflect.DeepEqual(f.public(t), before) || unchangedRefreshCounts(t, f) != counts {
		t.Fatal("successful unchanged cleanup invented public activity")
	}
}

func TestUnchangedPublicationRefreshFailurePreservesAuthoritativeWrite(t *testing.T) {
	f := newPublicationFixture(t)
	if w := f.apply(t, Approved{0, true, testPublicManifest()}, "publish"); w.Code != http.StatusOK {
		t.Fatal("publish", w.Code)
	}
	before := f.public(t)
	counts := unchangedRefreshCounts(t, f)
	hints, release, err := f.store.SubscribePublicProjection(f.project, "unchanged_refresh_fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	rejectUnchangedRefreshClear(t, f)
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET name='authoritative update survives unchanged projection fault' WHERE id=?`, f.project); err != nil {
			return err
		}
		return Refresh(ctx, tx, f.project, f.service.reader)
	}); err != nil {
		t.Fatal("optional projection blocked authoritative update", err)
	}
	var name, code string
	if err := f.store.DB().QueryRowContext(t.Context(), `SELECT name FROM projects WHERE id=?`, f.project).Scan(&name); err != nil || name != "authoritative update survives unchanged projection fault" {
		t.Fatal("authoritative update missing", err)
	}
	if err := f.store.DB().QueryRowContext(t.Context(), `SELECT refresh_error FROM project_publications WHERE project_id=?`, f.project).Scan(&code); err != nil || code != "projection_unavailable" {
		t.Fatal("optional failure not retained privately", err, code)
	}
	if !reflect.DeepEqual(f.public(t), before) || unchangedRefreshCounts(t, f) != counts {
		t.Fatal("contained unchanged cleanup failure changed public history or receipts")
	}
	select {
	case <-hints:
		t.Fatal("contained unchanged cleanup failure emitted a public event")
	default:
	}
}
