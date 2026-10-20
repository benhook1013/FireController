package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestCarriedObligationsRepositoryChangeHoldsAdmissionAndSelection(t *testing.T) {
	f := frontFixture(t)
	retainObligationSource(t, f, attemptProject, "repository-source-importer", "retained-source", strings.Repeat("c", 64), 0)
	admissionSQL(t, f, `UPDATE projects SET github_repository_id=999 WHERE id=?`, attemptProject)
	checkHold := func(err error) {
		t.Helper()
		var failure *core.Error
		if !errors.As(err, &failure) || failure.Code != "review_held" || failure.Status != 409 {
			t.Fatal("repository change became persistence failure", err)
		}
		details, ok := failure.Details.(map[string]string)
		if !ok || details["reason"] != "repository_changed" {
			t.Fatal("wrong protective hold", failure.Code, failure.Details)
		}
	}
	before := selectionCounts(t, f)
	_, err := f.e.Enqueue(t.Context(), f.scope(t, attemptProject, f.writer, false), "changed-repository-request", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, HeadSHA: strings.Repeat("a", 40), Channel: "cli"})
	checkHold(err)
	if before != selectionCounts(t, f) {
		t.Fatal("refused request persisted work or receipt")
	}
	err = f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := sourceObligationCount(ctx, tx, attemptProject, 2)
		return err
	})
	checkHold(err)
	reader := &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{Number: n, RepositoryID: 999, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done, err := f.e.StartReviewRuns(ctx, map[string]TargetReader{"test-capability": reader})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("selection worker stopped with persistence failure", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("selection workers did not stop")
		}
	})
	for _, key := range []string{"repository-change-first", "repository-change-next"} {
		receipt, _ := selectionEnqueue(t, f, key, "cli", nil)
		got, settled, err := f.e.WaitRun(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), receipt.ID, 2)
		if err != nil || !settled || got.State != "held" || got.Reason != "repository_changed" || got.OperationID != "" {
			t.Fatal("selection did not settle as repository hold", got, settled, err)
		}
		if front := frontChannel(t, frontRead(t, f), "cli"); front.State != "held" || front.Reason != "repository_changed" || front.PullNumber != 2 {
			t.Fatal("front lost repository-change diagnostic", front)
		}
	}
	var retainedRepository int64
	if err := f.store.DB().QueryRow(`SELECT repository_id FROM review_adoption_sources WHERE project_id=?`, attemptProject).Scan(&retainedRepository); err != nil || retainedRepository != 801 {
		t.Fatal("retained source rebound or lost", retainedRepository, err)
	}
	if reader.calls.Load() != 4 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("protective hold granted work or repeated source reads", reader.calls.Load())
	}
}
