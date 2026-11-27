package reviewexec

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestSelectedChildDoesNotSettleRearmedLaneBasis(t *testing.T) {
	for _, rearm := range []bool{false, true} {
		name := "stable-basis"
		if rearm {
			name = "rearmed-during-valid-read"
		}
		t.Run(name, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
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
				if err := core.AppendEvent(ctx, tx, scope, "review.scheduling_updated", scope.ProjectID, 1, map[string]any{"automatic_hosted": true, "automatic_cli": false, "hosted_basis_revision": 1, "cli_basis_revision": 1}); err != nil {
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
			edited := false
			reader := &refreshTestReader{repository: func(ctx context.Context, id int64) (reviewprovider.Repository, error) {
				if rearm && !edited {
					err := f.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
						if _, err := tx.ExecContext(ctx, `UPDATE review_scheduling SET revision=2, updated_at=? WHERE project_id=? AND revision=1`, stamp(), scope.ProjectID); err != nil {
							return err
						}
						return core.AppendEvent(ctx, tx, scope, "review.scheduling_updated", scope.ProjectID, 2, map[string]any{"automatic_hosted": true, "automatic_cli": false, "hosted_basis_revision": 2, "cli_basis_revision": 1})
					})
					if err != nil {
						return reviewprovider.Repository{}, err
					}
					edited = true
				}
				return reviewprovider.Repository{ID: id, FullName: "owner/repo", DefaultBranch: "main"}, nil
			}}
			selectionNext(t, f, reader)
			got := selectionRead(t, f, receipt.ID)
			if edited != rearm || got.State != "selected" || got.OperationID == "" {
				t.Fatalf("legitimate selection did not keep its linked child: rearm=%v callback=%v got=%+v", rearm, edited, got)
			}
			var settled, pullBasis string
			var pull int64
			if err := f.store.DB().QueryRow(`SELECT settled_basis_sha256,pull_number,pull_basis_sha256 FROM review_automatic_selections WHERE selection_id=? AND project_id=?`, receipt.ID, scope.ProjectID).Scan(&settled, &pull, &pullBasis); err != nil {
				t.Fatal(err)
			}
			if rearm {
				if settled != "" || pull != 0 || pullBasis != "" {
					t.Fatalf("old selection incorrectly marked a rearmed lane served")
				}
			} else if !hash.MatchString(settled) || pull != 2 || !hash.MatchString(pullBasis) {
				t.Fatalf("stable selection did not settle its own lane/pull basis")
			}
		})
	}
}
