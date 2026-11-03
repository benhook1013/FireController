package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func reviewWatchEvent(t *testing.T, f *attemptFixture, id, state string) {
	t.Helper()
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM review_operations WHERE id=?`, id).Scan(&revision); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE review_operations SET state=?,revision=revision+1 WHERE id=?`, state, id); err != nil {
			return err
		}
		return core.AppendEvent(ctx, tx, core.Scope{ProjectID: attemptProject, PrincipalID: "private-actor"}, "review.operation_updated", id, revision+1, struct{}{})
	})
	if err != nil {
		t.Fatal(err)
	}
}
func watchOptions() ReviewWatchOptions {
	return ReviewWatchOptions{Channel: "hosted", PullNumber: 2, Limit: 1}
}

func TestReviewWatchSnapshotReplayFiltersTerminalAndCursorBinding(t *testing.T) {
	f := operationListFixture(t)
	scope := operationReadScope(t, f)
	id := seedOperationListRow(t, f, 1, attemptProject, "queued", "hosted", 2)
	seedOperationListRow(t, f, 2, attemptProject, "queued", "hosted", 2)
	o := watchOptions()
	snapshot, err := f.e.WatchReviews(t.Context(), scope, o, 0)
	if err != nil || snapshot.Snapshot == nil || len(snapshot.Snapshot.Items) != 1 || !snapshot.Snapshot.HasMore || snapshot.Snapshot.Cursor == "" || snapshot.EventCursor == snapshot.Snapshot.Cursor {
		t.Fatal(snapshot, err)
	}
	// A commit after the atomic snapshot and before registration must replay.
	reviewWatchEvent(t, f, id, "held")
	o.After = snapshot.EventCursor
	changes, err := f.e.WatchReviews(t.Context(), scope, o, 0)
	if err != nil || len(changes.Changes) != 1 || changes.Changes[0].Operation.ID != id || changes.Changes[0].Operation.State != "held" || changes.TimedOut {
		t.Fatal(changes, err)
	}
	other := seedOperationListRow(t, f, 3, attemptProject, "queued", "cli", 2)
	reviewWatchEvent(t, f, other, "failed")
	seedOperationListRow(t, f, 4, otherAttemptProject, "queued", "hosted", 2)
	o.After = changes.EventCursor
	quiet, err := f.e.WatchReviews(t.Context(), scope, o, 0)
	if err != nil || len(quiet.Changes) != 0 || !quiet.TimedOut {
		t.Fatal(quiet, err)
	}
	for _, bad := range []ReviewWatchOptions{{PullNumber: 3, Channel: "hosted", Limit: 1, After: o.After}, {PullNumber: 2, Channel: "cli", Limit: 1, After: o.After}, {PullNumber: 2, Channel: "hosted", Limit: 1, After: snapshot.Snapshot.Cursor}} {
		if _, err = f.e.WatchReviews(t.Context(), scope, bad, 0); err == nil {
			t.Fatal("cross-filter/page cursor accepted", bad)
		}
	}
	foreign := scope
	foreign.ProjectID = otherAttemptProject
	if _, err = f.e.WatchReviews(t.Context(), foreign, o, 0); err == nil {
		t.Fatal("foreign project cursor accepted")
	}
	// New terminal transitions remain events although ordinary attention omits them.
	reviewWatchEvent(t, f, id, "failed")
	o.After = quiet.EventCursor
	failed, err := f.e.WatchReviews(t.Context(), scope, o, 0)
	if err != nil || len(failed.Changes) != 1 || failed.Changes[0].Operation.State != "failed" {
		t.Fatal(failed, err)
	}
	_, err = f.store.DB().Exec(`UPDATE events SET created_at='2000-01-01T00:00:00Z' WHERE event_type='review.operation_updated'`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.e.WatchReviews(t.Context(), scope, o, 0)
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "resync_required" {
		t.Fatal("expiry lost", err)
	}
}

func TestReviewWatchQuietStreamCancellationAndCurrentExecutionWire(t *testing.T) {
	f := operationListFixture(t)
	scope := operationReadScope(t, f)
	o := watchOptions()
	id := seedOperationListRow(t, f, 1, attemptProject, "queued", "hosted", 2)
	snapshot, err := f.e.WatchReviews(t.Context(), scope, o, 0)
	if err != nil {
		t.Fatal(err)
	}
	o.After = snapshot.EventCursor
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.e.WatchReviews(ctx, scope, o, 900); done <- err }()
	// An unrelated real project event must not end the quiet wait.
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return core.AppendEvent(ctx, tx, scope, "job.updated", "job-unrelated", 1, struct{}{})
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatal("unrelated event ended wait", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation succeeded as a result")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled wait remained active")
	}
	// A new wait over the original cursor still sees later committed work.
	reviewWatchEvent(t, f, id, "failed")
	result, err := f.e.WatchReviews(t.Context(), scope, o, 900)
	if err != nil || len(result.Changes) != 1 {
		t.Fatal(result, err)
	}
	path := "/api/v1/projects/" + attemptProject + "/reviews/watch?pull_number=2&channel=hosted&limit=0&after=" + url.QueryEscape(result.EventCursor)
	w := f.request(http.MethodGet, path, f.reader, "", "")
	if w.Code != 400 {
		t.Fatal("invalid selector not refused", w.Code, w.Body.String())
	}
	config := f.request(http.MethodGet, "/api/v1/projects/"+attemptProject+"/reviews/execution/configuration", f.reader, "", "")
	var keys map[string]json.RawMessage
	if config.Code != 200 || json.Unmarshal(config.Body.Bytes(), &keys) != nil || len(keys) != 7 || strings.Contains(config.Body.String(), "automatic_hosted") {
		t.Fatal("old execution wire changed", config.Code, config.Body.String())
	}
}

func TestReviewWatchInitialReadLifetimeRetainsReconnectClassification(t *testing.T) {
	for _, phase := range []string{"already_cancelled", "during_read"} {
		t.Run(phase, func(t *testing.T) {
			f := operationListFixture(t)
			scope := operationReadScope(t, f)
			o := watchOptions()
			snapshot, err := f.e.WatchReviews(t.Context(), scope, o, 0)
			if err != nil {
				t.Fatal(err)
			}
			o.After = snapshot.EventCursor
			lifetime, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.e.resultWaitLifetime = lifetime
			before := attemptCount(t, f.store, `SELECT count(*) FROM events`)
			var release func()
			if phase == "already_cancelled" {
				cancel()
			} else {
				release = holdCIWatchReadPool(t, f)
				defer release()
			}
			blocked := f.store.DB().Stats().WaitCount
			done := make(chan error, 1)
			go func() { _, err := f.e.WatchReviews(t.Context(), scope, o, 900); done <- err }()
			if phase == "during_read" {
				deadline := time.Now().Add(time.Second)
				for f.store.DB().Stats().WaitCount == blocked && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if f.store.DB().Stats().WaitCount == blocked {
					t.Fatal("fixture never reached initial blocked read")
				}
				cancel()
			}
			select {
			case err := <-done:
				var failure *core.Error
				if !errors.As(err, &failure) || failure.Code != "overloaded" {
					t.Fatal(err)
				}
				details, ok := failure.Details.(map[string]string)
				if !ok || details["reason"] != "result_wait_lifetime" {
					t.Fatal("restart classification lost", failure)
				}
			case <-time.After(time.Second):
				t.Fatal("lifetime did not release initial read")
			}
			if release != nil {
				release()
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM events`) != before {
				t.Fatal("wait changed durable events")
			}
		})
	}
}

func TestReviewWatchReplayBoundAndPageLimitMayChange(t *testing.T) {
	f := operationListFixture(t)
	scope := operationReadScope(t, f)
	o := watchOptions()
	id := seedOperationListRow(t, f, 1, attemptProject, "queued", "hosted", 2)
	initial, err := f.e.WatchReviews(t.Context(), scope, o, 0)
	if err != nil {
		t.Fatal(err)
	}
	reviewWatchEvent(t, f, id, "held")
	reviewWatchEvent(t, f, id, "failed")
	o.After = initial.EventCursor
	first, err := f.e.WatchReviews(t.Context(), scope, o, 0)
	if err != nil || len(first.Changes) != 1 || !first.HasMore || first.Changes[0].Revision != 2 || first.Changes[0].Operation.Revision != 3 {
		t.Fatal(first, err)
	}
	o.After = first.EventCursor
	o.Limit = 2
	second, err := f.e.WatchReviews(t.Context(), scope, o, 0)
	if err != nil || len(second.Changes) != 1 || second.HasMore || second.Changes[0].Revision != 3 || second.Changes[0].Sequence <= first.Changes[0].Sequence {
		t.Fatal(second, err)
	}
	o.After = second.EventCursor
	quiet, err := f.e.WatchReviews(t.Context(), scope, o, 0)
	if err != nil || !quiet.TimedOut || len(quiet.Changes) != 0 {
		t.Fatal(quiet, err)
	}
}
