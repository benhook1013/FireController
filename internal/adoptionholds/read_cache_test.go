package adoptionholds_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	h "github.com/benhook1013/FireController/internal/adoptionholds"
	m "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/core"
)

type obligationCountingQuery struct {
	*sql.DB
	source, domain, project, originals, targets, resolutions int
}

func (q *obligationCountingQuery) count(query string) {
	switch {
	case strings.Contains(query, "SELECT repository_id,snapshot_sha256,revision FROM review_adoption_sources"):
		q.source++
	case strings.Contains(query, "schema:review_adoption_obligations"):
		q.domain++
	case strings.Contains(query, "SELECT github_repository_id FROM projects"):
		q.project++
	case strings.Contains(query, "SELECT id,project_id,namespace,source_revision"):
		q.originals++
	case strings.Contains(query, "SELECT project_id,pull_number FROM review_adoption_targets"):
		q.targets++
	case strings.Contains(query, "SELECT revision,reason,evidence_json"):
		q.resolutions++
	}
}
func (q *obligationCountingQuery) QueryRowContext(ctx context.Context, s string, args ...any) *sql.Row {
	q.count(s)
	return q.DB.QueryRowContext(ctx, s, args...)
}
func (q *obligationCountingQuery) QueryContext(ctx context.Context, s string, args ...any) (*sql.Rows, error) {
	q.count(s)
	return q.DB.QueryContext(ctx, s, args...)
}

func TestReadFactsNamespaceCacheChecksEveryOriginalAndRefreshesNextCall(t *testing.T) {
	f := fresh(t)
	for _, namespace := range []string{"source-a", "source-b"} {
		b := batch()
		b.Source.Namespace = namespace
		b.Obligations = nil
		for i := 0; i < 70; i++ {
			b.Obligations = append(b.Obligations, m.Obligation{SourceKey: fmt.Sprintf("finding-%03d", i), Kind: "finding", AffectedPullNumbers: []int64{3251}, Summary: "Retained original", Evidence: []string{}, OriginalStatus: "open"})
		}
		f.apply(b, namespace)
	}
	q := &obligationCountingQuery{DB: f.store.DB()}
	facts, err := h.ReadFacts(t.Context(), q, "project_one", 3251)
	if err != nil || facts.Open != 140 {
		t.Fatal("scoped facts", facts, err)
	}
	if q.source != 2 || q.project != 2 || q.domain != 3 {
		t.Fatalf("namespace validation repeated per original: source=%d project=%d domain=%d", q.source, q.project, q.domain)
	}
	if q.originals != 140 || q.targets != 140 || q.resolutions != 140 {
		t.Fatalf("cache skipped original/target/resolution validation: %+v", q)
	}
	// A second invocation must revalidate even when all namespaces matched before.
	f.exec(`UPDATE projects SET github_repository_id=19 WHERE id='project_one'`)
	_, err = h.ReadFacts(t.Context(), q, "project_one", 3251)
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "repository_changed" || failure.Status != 409 {
		t.Fatal("repository change concealed by cache/wrapper", err)
	}
}

func TestReadFactsCachedSourceDoesNotHideLaterCorruption(t *testing.T) {
	f := fresh(t)
	b := batch()
	b.Obligations = []m.Obligation{}
	for i := 0; i < 70; i++ {
		b.Obligations = append(b.Obligations, m.Obligation{SourceKey: fmt.Sprintf("finding-%03d", i), Kind: "finding", AffectedPullNumbers: []int64{3251}, Summary: "Retained original", Evidence: []string{}, OriginalStatus: "open"})
	}
	f.apply(b, "many-originals")
	for _, tc := range []struct{ name, sql string }{
		{"original", `DROP TRIGGER review_adoption_originals_immutable; UPDATE review_adoption_obligations SET original_sha256='invalid' WHERE id=(SELECT max(id) FROM review_adoption_obligations)`},
		{"targets", `INSERT INTO review_adoption_targets SELECT id,project_id,999 FROM review_adoption_obligations WHERE id=(SELECT max(id) FROM review_adoption_obligations)`},
		{"resolution", `UPDATE review_adoption_obligations SET revision=2 WHERE id=(SELECT max(id) FROM review_adoption_obligations)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checked := false
			var validationErr error
			_ = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, tc.sql); err != nil {
					return err
				}
				checked = true
				_, validationErr = h.ReadFacts(ctx, tx, "project_one", 3251)
				if validationErr != nil {
					return validationErr
				}
				return errors.New("force fixture rollback after unexpected acceptance")
			})
			var failure *core.Error
			if !checked || !errors.As(validationErr, &failure) || failure.Code != "storage_unavailable" {
				t.Fatal("later corrupt record did not fail closed", checked, validationErr)
			}
			if facts, err := h.ReadFacts(t.Context(), f.store.DB(), "project_one", 3251); err != nil || facts.Open != 70 {
				t.Fatal("failed read transaction changed retained state", facts, err)
			}
		})
	}
}

func TestReadFactsRepositoryChangeClassificationSurvivesWrappers(t *testing.T) {
	f := fresh(t)
	f.apply(batch(), "retained-source")
	f.exec(`UPDATE projects SET github_repository_id=19 WHERE id='project_one'`)
	checks := []struct {
		name string
		read func() error
	}{
		{"source", func() error {
			_, err := h.ReadSource(t.Context(), f.store.DB(), "project_one", "sealed-source")
			return err
		}},
		{"facts", func() error { _, err := h.ReadFacts(t.Context(), f.store.DB(), "project_one", 3251); return err }},
		{"open", func() error { _, err := h.HasOpen(t.Context(), f.store.DB(), "project_one", 3251); return err }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			var failure *core.Error
			err := check.read()
			if !errors.As(err, &failure) || failure.Code != "repository_changed" || failure.Status != 409 {
				t.Fatal("repository mismatch lost its classification", err)
			}
		})
	}
}

func TestReadFactsNamespaceCacheRemainsBounded(t *testing.T) {
	f := fresh(t)
	for n := 0; n < 65; n++ {
		b := batch()
		b.Source.Namespace = fmt.Sprintf("bounded-source-%02d", n)
		for i := range b.Obligations {
			b.Obligations[i].AffectedPullNumbers = []int64{3251}
		}
		f.apply(b, b.Source.Namespace)
	}
	q := &obligationCountingQuery{DB: f.store.DB()}
	facts, err := h.ReadFacts(t.Context(), q, "project_one", 3251)
	if err != nil || facts.Open != 130 {
		t.Fatal("overflow namespaces lost obligations", facts, err)
	}
	// 64 namespaces are cached; the remaining namespace is validated for each
	// original. This remains true regardless of random obligation-ID ordering.
	if q.source != 66 || q.project != 66 || q.domain != 67 {
		t.Fatalf("cache exceeded 64 namespaces or skipped overflow validation: source=%d project=%d domain=%d", q.source, q.project, q.domain)
	}
	if q.originals != 130 || q.targets != 130 || q.resolutions != 130 {
		t.Fatalf("overflow cache skipped record validation: %+v", q)
	}
}
