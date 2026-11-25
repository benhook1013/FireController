package reviewexec

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestAutomaticSourceHoldRetainsOnlyItsBoundPullSchedulingBasis(t *testing.T) {
	for _, edited := range []bool{false, true} {
		name := "same-pull-revision"
		if edited {
			name = "lawful-pause-and-resume-during-source-read"
		}
		t.Run(name, func(t *testing.T) {
			f := schedulingFixture(t)
			scope := selectionScope(t, f, attemptProject, f.admin, "write")
			var receipt Receipt
			err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				var credential string
				if err := tx.QueryRowContext(ctx, `SELECT id FROM credentials WHERE principal_id=? AND is_admin=1 AND revoked=0 ORDER BY id LIMIT 1`, scope.PrincipalID).Scan(&credential); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO review_scheduling(project_id,revision,automatic_hosted,automatic_cli,actor_id,worker_id,credential_id,updated_at) VALUES(?,1,1,0,?,?,?,?)`, scope.ProjectID, scope.PrincipalID, scope.WorkerID, credential, stamp()); err != nil {
					return err
				}
				if err := core.AppendEvent(ctx, tx, scope, "review.scheduling_updated", scope.ProjectID, 1, map[string]any{"automatic_hosted": true, "automatic_cli": false}); err != nil {
					return err
				}
				var err error
				receipt, err = f.e.enqueueRunTx(ctx, tx, scope, RunRequest{Channel: "hosted", origin: selectionOriginAutomatic})
				if err != nil {
					return err
				}
				_, err = tx.ExecContext(ctx, `INSERT INTO review_automatic_selections(selection_id,project_id,scheduling_revision,basis_sha256,actor_id,worker_id,credential_id) VALUES(?,?,1,?,?,?,?)`, receipt.ID, scope.ProjectID, strings.Repeat("a", 64), scope.PrincipalID, scope.WorkerID, credential)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			reader := &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
				if edited {
					schedulingPause(t, f, 2, 0, true)
					schedulingPause(t, f, 2, 1, false)
				}
				return reviewprovider.Repository{}, context.DeadlineExceeded
			}}
			selectionNext(t, f, reader)
			got := selectionRead(t, f, receipt.ID)
			if got.State != "held" || got.Reason != "source_fetch_timeout" || got.OperationID != "" || reader.calls.Load() != 1 {
				t.Fatalf("source hold changed: %+v; reads=%d", got, reader.calls.Load())
			}
			var settled string
			if err := f.store.DB().QueryRow(`SELECT settled_basis_sha256 FROM review_automatic_selections WHERE selection_id=?`, receipt.ID).Scan(&settled); err != nil {
				t.Fatal(err)
			}
			if edited && settled != "" || !edited && !hash.MatchString(settled) {
				t.Fatalf("unexpected settled basis after lawful source hold: edited=%v basis=%q", edited, settled)
			}
		})
	}
}
