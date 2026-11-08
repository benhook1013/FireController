package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type nativeBudgetQuery struct {
	EvidenceQuery
	match            string
	occurrence, hits int
	fail             func(context.Context) (context.Context, *sql.DB)
}

func (q *nativeBudgetQuery) failed(ctx context.Context, query string) (context.Context, *sql.DB) {
	if strings.Contains(query, q.match) {
		q.hits++
		if q.hits == q.occurrence {
			return q.fail(ctx)
		}
	}
	return ctx, nil
}
func (q *nativeBudgetQuery) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	ctx, db := q.failed(ctx, query)
	if db != nil {
		return db.QueryContext(ctx, query, args...)
	}
	return q.EvidenceQuery.QueryContext(ctx, query, args...)
}
func (q *nativeBudgetQuery) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	ctx, db := q.failed(ctx, query)
	if db != nil {
		return db.QueryRowContext(ctx, query, args...)
	}
	return q.EvidenceQuery.QueryRowContext(ctx, query, args...)
}

func TestExternalNativeBudgetPerRecordFailureIsolation(t *testing.T) {
	f, a, in := hostedPublicationFixture(t, true)
	if _, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "captured", ""); err != nil {
		t.Fatal(err)
	}
	target := lifecycleTarget{project: attemptProject, repository: a.Anchor.RepositoryID, number: 2}
	run := reviewprovider.ExternalHostedRun{Inventory: reviewprovider.HostedFindingInventory{Publication: reviewprovider.HostedPublication{PublicationID: in.Hosted.PublicationID}}}
	for _, read := range []struct {
		name, match string
		occurrence  int
	}{
		{"indexed lookup", "INDEXED BY review_evidence_hosted_publication", 1},
		{"operation", "FROM review_operations", 1},
		{"attempt", "FROM review_execution_attempts", 1},
		{"recovery evidence", "SELECT id,project_id,operation_id FROM review_execution_evidence", 1},
		{"recovery adjudication", "SELECT e.body_json,s.revision,s.body_json", 1},
		{"final adjudication", "SELECT e.body_json,s.revision,s.body_json", 2},
	} {
		for _, mode := range []string{"local deadline", "parent cancellation", "storage after deadline"} {
			t.Run(read.name+"/"+mode, func(t *testing.T) {
				parent, parentCancel := context.WithCancel(t.Context())
				defer parentCancel()
				// The test controls local budget expiry at the selected query without sleeping.
				budget, budgetCancel := context.WithCancel(parent)
				defer budgetCancel()
				expired := context.WithValue(budget, struct{}{}, true)
				readCtx := &nativeDeadlineContext{Context: expired}
				storage := errors.New("controlled genuine driver failure")
				failedDB := sql.OpenDB(roundAdoptionFailureConnector{err: storage})
				defer failedDB.Close()
				q := &nativeBudgetQuery{EvidenceQuery: f.store.DB(), match: read.match, occurrence: read.occurrence, fail: func(ctx context.Context) (context.Context, *sql.DB) {
					if mode == "parent cancellation" {
						parentCancel()
					} else {
						budgetCancel()
					}
					if mode == "storage after deadline" {
						return context.Background(), failedDB
					}
					return ctx, nil
				}}
				captures, held, err := externalNativeCapturesWithinBudget(parent, readCtx, q, target, run)
				if q.hits != read.occurrence || len(captures) != 0 {
					t.Fatal("did not reach selected read", q.hits, captures, err)
				}
				switch mode {
				case "local deadline":
					if !held || err != nil {
						t.Fatal("local read expiry escaped target hold", held, err)
					}
				case "parent cancellation":
					if held || !errors.Is(err, context.Canceled) {
						t.Fatal("parent cancellation swallowed", held, err)
					}
				case "storage after deadline":
					if held || !errors.Is(err, storage) {
						t.Fatal("real driver failure swallowed by coincident expiry", held, err)
					}
				}
			})
		}
	}
}

// The test controls when the local deadline expires, including mid-read. The
// production path still owns a five-second WithTimeout and uses the same body.
type nativeDeadlineContext struct{ context.Context }

func (c *nativeDeadlineContext) Err() error {
	if c.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}
func (c *nativeDeadlineContext) Deadline() (time.Time, bool) { return time.Now().Add(time.Hour), true }

func TestExternalNativeBudgetRealMissingTableIsFatal(t *testing.T) {
	f, a, in := hostedPublicationFixture(t, true)
	if _, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "captured", ""); err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `ALTER TABLE review_operations RENAME TO unavailable_operations`)
	_, held, err := externalNativeCaptures(t.Context(), f.store.DB(), lifecycleTarget{project: attemptProject, repository: a.Anchor.RepositoryID, number: 2}, reviewprovider.ExternalHostedRun{Inventory: reviewprovider.HostedFindingInventory{Publication: reviewprovider.HostedPublication{PublicationID: in.Hosted.PublicationID}}})
	if held || err == nil {
		t.Fatal("real SQLite error swallowed", held, err)
	}
}
