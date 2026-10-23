package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func selectionFixture(t *testing.T) *attemptFixture {
	return selectionFixtureWithQueue(t, []int64{2, 4})
}
func selectionFixtureWithQueue(t *testing.T, queue []int64) *attemptFixture {
	t.Helper()
	f := newAttemptFixture(t)
	zero := int64(0)
	seedLegacyExecutionConfiguration(t, f, attemptProject, "config-exec-project-one-0", Configuration{ExpectedRevision: &zero, CapabilityID: "test-capability", Mode: "independent", Queue: queue, Channels: []string{"cli", "hosted"}})
	for _, p := range []string{attemptProject, otherAttemptProject} {
		if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,'selection-worker','Selector')`, p); err != nil {
			t.Fatal(err)
		}
	}
	return f
}
func selectionScope(t *testing.T, f *attemptFixture, project, token, permission string) core.Scope {
	t.Helper()
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("project_id", project)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-FC-Worker", "selection-worker")
	scope, err := f.store.Scope(r, permission)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}
func selectionEnqueue(t *testing.T, f *attemptFixture, key, channel string, expected *int64) (Receipt, []byte) {
	t.Helper()
	raw, err := f.e.EnqueueRun(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), key, RunRequest{Channel: channel, ExpectedPullNumber: expected})
	if err != nil {
		t.Fatal(err)
	}
	var r Receipt
	if json.Unmarshal(raw, &r) != nil {
		t.Fatal("bad receipt")
	}
	return r, raw
}
func selectionRead(t *testing.T, f *attemptFixture, id string) Run {
	t.Helper()
	r, err := f.e.ReadRun(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func selectionNext(t *testing.T, f *attemptFixture, reader TargetReader) {
	t.Helper()
	worked, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": reader})
	if err != nil || !worked {
		t.Fatal(worked, err)
	}
}
func selectionCounts(t *testing.T, f *attemptFixture) string {
	t.Helper()
	var values []int
	for _, table := range []string{"review_selections", "review_operations", "review_execution_attempts", "events", "receipts"} {
		var n int
		if err := f.store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		values = append(values, n)
	}
	return fmt.Sprint(values)
}
func TestRunSelectionOneChildReplayRestartAndOriginalBinding(t *testing.T) {
	f := selectionFixture(t)
	reader := &refreshTestReader{}
	r, original := selectionEnqueue(t, f, "one-call", "cli", nil)
	if reader.calls.Load() != 0 || r.State != "queued" || r.Revision != 1 {
		t.Fatal("enqueue performed source work")
	}
	before := selectionCounts(t, f)
	selectionNext(t, f, reader)
	got := selectionRead(t, f, r.ID)
	if got.State != "selected" || got.OperationID == "" || got.Revision != 3 || got.WorkerID != "selection-worker" || reader.calls.Load() != 2 {
		t.Fatal("selection did not create exactly one bounded child", got, reader.calls.Load())
	}
	op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), got.OperationID)
	if err != nil || op.PullNumber != 2 || op.HeadSHA != strings.Repeat("a", 40) || op.WorkerID != got.WorkerID || op.ActorID != got.ActorID || op.Channel != "cli" {
		t.Fatal("child identity changed", op, err)
	}
	counts := selectionCounts(t, f)
	if counts == before {
		t.Fatal("no durable child")
	}
	worked, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": reader})
	if err != nil || worked || selectionCounts(t, f) != counts || reader.calls.Load() != 2 {
		t.Fatal("completion autonomously repeated selection", err)
	}
	f.configure(t, attemptProject, "test-capability", 1, false)
	var database string
	rows, err := f.store.DB().Query(`PRAGMA database_list`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var seq int
		var name string
		if err = rows.Scan(&seq, &name, &database); err != nil {
			t.Fatal(err)
		}
		if name == "main" {
			break
		}
	}
	rows.Close()
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(t.Context(), filepath.Dir(database))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	f.store = reopened
	f.e, err = New(reopened, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := f.e.EnqueueRun(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "one-call", RunRequest{Channel: "cli"})
	if err != nil || !bytes.Equal(raw, original) {
		t.Fatal("original key rebound after later config/restart", err)
	}
	if err = f.e.RecoverReviewRuns(t.Context()); err != nil {
		t.Fatal(err)
	}
	again := selectionRead(t, f, r.ID)
	if !reflect.DeepEqual(got, again) {
		t.Fatal("restart changed selected child")
	}
	_, err = f.e.EnqueueRun(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "one-call", RunRequest{Channel: "hosted"})
	attemptCode(t, err, "idempotency_conflict")
}
func TestRunSelectionAuthorityCASAndPendingChannels(t *testing.T) {
	f := selectionFixture(t)
	scope := selectionScope(t, f, attemptProject, f.writer, "write")
	before := selectionCounts(t, f)
	_, err := f.e.EnqueueRun(t.Context(), f.scope(t, attemptProject, f.writer, false), "missing-worker", RunRequest{Channel: "cli"})
	attemptCode(t, err, "validation_failed")
	stale := int64(2)
	_, err = f.e.EnqueueRun(t.Context(), scope, "stale", RunRequest{Channel: "cli", ExpectedConfigurationRevision: &stale})
	attemptCode(t, err, "review_conflict")
	_, err = f.e.EnqueueRun(t.Context(), selectionScope(t, f, attemptProject, f.reader, "read"), "read-only", RunRequest{Channel: "cli"})
	attemptCode(t, err, "forbidden")
	if selectionCounts(t, f) != before {
		t.Fatal("rejected call changed state")
	}
	first, _ := selectionEnqueue(t, f, "cli", "cli", nil)
	_, err = f.e.EnqueueRun(t.Context(), scope, "cli-other", RunRequest{Channel: "cli"})
	attemptCode(t, err, "review_busy")
	selectionEnqueue(t, f, "hosted", "hosted", nil)
	_, err = f.e.ReadRun(t.Context(), selectionScope(t, f, otherAttemptProject, f.other, "read"), first.ID)
	attemptCode(t, err, "not_found")
	if _, err = f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`); err != nil {
		t.Fatal(err)
	}
	_, err = f.e.EnqueueRun(t.Context(), scope, "cli", RunRequest{Channel: "cli"})
	attemptCode(t, err, "unauthorized")
	reader := &refreshTestReader{}
	selectionNext(t, f, reader)
	var state, reason string
	if err = f.store.DB().QueryRow(`SELECT state,reason FROM review_selections WHERE id=?`, first.ID).Scan(&state, &reason); err != nil || state != "held" || reason != "authorization_changed" || reader.calls.Load() != 0 {
		t.Fatal(state, reason, reader.calls.Load(), err)
	}
}
func TestRunSelectionFrontPrefixAndExpectedAssertion(t *testing.T) {
	for _, mode := range []string{"first", "merged-prefix", "closed-prefix", "stopped-prefix", "expected-mismatch", "all-merged"} {
		t.Run(mode, func(t *testing.T) {
			f := selectionFixture(t)
			if mode == "stopped-prefix" {
				zero := int64(0)
				_, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "close-front", 2, "cli", RoundControlRequest{ExpectedRevision: &zero, Action: "close"})
				if err != nil {
					t.Fatal(err)
				}
			}
			var calls []int64
			reader := &refreshTestReader{pull: func(ctx context.Context, _ string, n int64) (reviewprovider.Pull, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 15*time.Second {
					t.Error("source lacks overall bounded context")
				}
				calls = append(calls, n)
				p := reviewprovider.Pull{Number: n, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}
				if n == 2 && mode == "merged-prefix" || mode == "all-merged" {
					p.State = "closed"
					p.Merged = true
				}
				if n == 2 && (mode == "closed-prefix" || mode == "stopped-prefix") {
					p.State = "closed"
				}
				return p, nil
			}}
			var expected *int64
			if mode == "expected-mismatch" {
				n := int64(4)
				expected = &n
			}
			r, _ := selectionEnqueue(t, f, "one", "cli", expected)
			selectionNext(t, f, reader)
			got := selectionRead(t, f, r.ID)
			want := int64(2)
			wantCalls := []int64{2}
			if mode == "merged-prefix" || mode == "stopped-prefix" || mode == "closed-prefix" {
				want = 4
				wantCalls = []int64{2, 4}
			}
			switch mode {
			case "expected-mismatch":
				if got.State != "held" || got.Reason != "expected_pr_mismatch" {
					t.Fatal(got)
				}
			case "all-merged":
				wantCalls = []int64{2, 4}
				if got.State != "empty" || got.Reason != "queue_complete" {
					t.Fatal(got)
				}
			default:
				op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), got.OperationID)
				if err != nil || got.State != "selected" || op.PullNumber != want {
					t.Fatal(got, op, err)
				}
			}
			if !reflect.DeepEqual(calls, wantCalls) || reader.calls.Load() != int64(1+len(wantCalls)) {
				t.Fatal("selection read suffix or repeated identities", calls, reader.calls.Load())
			}
		})
	}
}
func TestRunSelectionPendingNativeEvidenceAndLiveActionHold(t *testing.T) {
	for _, mode := range []string{"queued-native", "pending-capture", "dirty-proof"} {
		t.Run(mode, func(t *testing.T) {
			queue := []int64{2, 4}
			if mode == "pending-capture" {
				// Exercise this target's pending capture, not unrelated independent
				// queue work that is now correctly allowed to progress.
				queue = []int64{2}
			}
			f := selectionFixtureWithQueue(t, queue)
			if mode == "queued-native" {
				f.enqueue(t, attemptProject, "native", "cli", 2, 1)
			} else {
				a, _ := roundCapture(t, f, 1, false)
				zero := int64(0)
				_, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "stop", 2, "cli", RoundControlRequest{ExpectedRevision: &zero, Action: "close"})
				if err != nil {
					t.Fatal(err)
				}
				if mode == "dirty-proof" {
					if _, err = f.store.DB().Exec(`UPDATE review_operations SET head_sha=? WHERE id=?`, strings.Repeat("f", 40), a.OperationID); err != nil {
						t.Fatal(err)
					}
				}
			}
			r, _ := selectionEnqueue(t, f, "one", "cli", nil)
			reader := &refreshTestReader{}
			selectionNext(t, f, reader)
			got := selectionRead(t, f, r.ID)
			reason := "review_inflight"
			if mode == "pending-capture" {
				reason = "review_evidence_pending"
			}
			if mode == "dirty-proof" {
				reason = "review_evidence_unavailable"
			}
			if got.State != "held" || got.Reason != reason {
				t.Fatal(got)
			}
			if mode == "queued-native" && reader.calls.Load() != 0 {
				t.Fatal("known native inflight performed source reads")
			}
		})
	}
}
func TestRunSelectionRechecksAfterSourceAndRollsBackLinkedChild(t *testing.T) {
	for _, mode := range []string{"configuration", "revoked", "deleted-worker", "round-close", "event-failure"} {
		t.Run(mode, func(t *testing.T) {
			f := selectionFixture(t)
			r, _ := selectionEnqueue(t, f, "one", "cli", nil)
			reader := &refreshTestReader{}
			reader.pull = func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
				switch mode {
				case "configuration":
					f.configure(t, attemptProject, "test-capability", 1, false)
				case "revoked":
					if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`); err != nil {
						t.Fatal(err)
					}
				case "deleted-worker":
					if _, err := f.store.DB().Exec(`DELETE FROM workers WHERE project_id=? AND id='selection-worker'`, attemptProject); err != nil {
						t.Fatal(err)
					}
				case "round-close":
					zero := int64(0)
					if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "closed-during-read", 2, "cli", RoundControlRequest{ExpectedRevision: &zero, Action: "close"}); err != nil {
						t.Fatal(err)
					}
				case "event-failure":
					if _, err := f.store.DB().Exec(`CREATE TRIGGER selection_fault BEFORE INSERT ON events WHEN NEW.event_type='review.selection_updated' AND json_extract(NEW.payload_json,'$.state')='selected' BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
						t.Fatal(err)
					}
				}
				return reviewprovider.Pull{Number: n, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
			}
			// The source phase can still finish local classification; a changed grant
			// may finish the whole queue only if the remaining prefix is also complete.
			if mode == "round-close" {
				reader.pull = func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
					zero := int64(0)
					if n == 2 {
						if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "closed-during-read", 2, "cli", RoundControlRequest{ExpectedRevision: &zero, Action: "close"}); err != nil {
							t.Fatal(err)
						}
					}
					return reviewprovider.Pull{Number: n, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
				}
			}
			worked, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": reader})
			if !worked {
				t.Fatal("no selection work")
			}
			var native int
			if e := f.store.DB().QueryRow(`SELECT count(*) FROM review_operations`).Scan(&native); e != nil {
				t.Fatal(e)
			}
			if mode == "event-failure" {
				if err == nil || native != 0 {
					t.Fatal("failed link/event committed orphan child", native, err)
				}
				got := selectionRead(t, f, r.ID)
				if got.State != "selecting" {
					t.Fatal(got)
				}
				if _, err = f.store.DB().Exec(`DROP TRIGGER selection_fault`); err != nil {
					t.Fatal(err)
				}
				if err = f.e.RecoverReviewRuns(t.Context()); err != nil {
					t.Fatal(err)
				}
				selectionNext(t, f, &refreshTestReader{})
				if selectionRead(t, f, r.ID).State != "selected" {
					t.Fatal("recovery failed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got Run
			if err = f.store.DB().QueryRow(`SELECT state,reason FROM review_selections WHERE id=?`, r.ID).Scan(&got.State, &got.Reason); err != nil {
				t.Fatal(err)
			}
			if mode == "round-close" {
				if got.State != "selected" || native != 1 {
					t.Fatal(got, native)
				}
				return
			}
			if got.State != "held" || native != 0 {
				t.Fatal(got, native)
			}
		})
	}
}
func TestRunSelectionWorkerWaitCancelAndRecovery(t *testing.T) {
	f := selectionFixture(t)
	r, _ := selectionEnqueue(t, f, "one", "cli", nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	reader := &refreshTestReader{repository: func(ctx context.Context, id int64) (reviewprovider.Repository, error) {
		once.Do(func() { close(entered) })
		select {
		case <-ctx.Done():
			return reviewprovider.Repository{}, ctx.Err()
		case <-release:
		}
		return reviewprovider.Repository{ID: id, FullName: "owner/repo", DefaultBranch: "main"}, nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done, err := f.e.StartReviewRuns(ctx, map[string]TargetReader{"test-capability": reader})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker never entered")
	}
	// Provider wait owns no SQLite transaction or write admission.
	if err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO workers VALUES(?,'during-wait','During wait')`, attemptProject)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	waitCtx, detach := context.WithCancel(t.Context())
	waitDone := make(chan error, 1)
	go func() {
		_, _, err := f.e.WaitRun(waitCtx, selectionScope(t, f, attemptProject, f.writer, "read"), r.ID, 30)
		waitDone <- err
	}()
	detach()
	attemptCode(t, <-waitDone, "overloaded")
	close(release)
	got, terminal, err := f.e.WaitRun(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), r.ID, 2)
	if err != nil || !terminal || got.State != "selected" {
		t.Fatal(got, terminal, err)
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	before := selectionCounts(t, f)
	if err = f.e.RecoverReviewRuns(t.Context()); err != nil || selectionCounts(t, f) != before {
		t.Fatal("recovery duplicated selected work", err)
	}
}

func TestRunSelectionRequiresConfiguredPolicyBeforeAcceptance(t *testing.T) {
	f := newAttemptFixtureWithPolicy(t, false)
	f.configure(t, attemptProject, "test-capability", 0, false)
	if _, err := f.store.DB().Exec(`INSERT INTO workers VALUES(?,'selection-worker','Selector')`, attemptProject); err != nil {
		t.Fatal(err)
	}
	before := selectionCounts(t, f)
	_, err := f.e.EnqueueRun(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "unconfigured-policy", RunRequest{Channel: "cli"})
	attemptCode(t, err, "reviews_policy_unconfigured")
	if selectionCounts(t, f) != before {
		t.Fatal("unconfigured policy reached schema/FK write")
	}
}
func TestRunSelectionRecoveryOfInterruptedReadAndWaitReauthorization(t *testing.T) {
	f := selectionFixture(t)
	r, _ := selectionEnqueue(t, f, "one", "cli", nil)
	// A genuine claim followed by caller cancellation leaves only a read-only
	// selecting row; startup may safely repeat it because no child was inserted.
	ctx, cancel := context.WithCancel(t.Context())
	reader := &refreshTestReader{repository: func(ctx context.Context, id int64) (reviewprovider.Repository, error) {
		cancel()
		return reviewprovider.Repository{}, ctx.Err()
	}}
	if _, err := f.e.runNext(ctx, map[string]TargetReader{"test-capability": reader}); err != nil {
		t.Fatal(err)
	}
	if selectionRead(t, f, r.ID).State != "selecting" {
		t.Fatal("interrupted source unexpectedly selected a target")
	}
	if err := f.e.RecoverReviewRuns(t.Context()); err != nil {
		t.Fatal(err)
	}
	if selectionRead(t, f, r.ID).State != "queued" {
		t.Fatal("read-only recovery did not preserve original identity")
	}
	scope := selectionScope(t, f, attemptProject, f.writer, "read")
	result := make(chan error, 1)
	go func() { _, _, err := f.e.WaitRun(t.Context(), scope, r.ID, 2); result <- err }()
	// Wait until subscribed so this exercises fresh authorization on notification.
	deadline := time.Now().Add(time.Second)
	for {
		f.e.operationMu.Lock()
		registered := len(f.e.operationWaiters[r.ID]) > 0
		f.e.operationMu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("wait never subscribed")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`); err != nil {
		t.Fatal(err)
	}
	f.e.notifyOperation(r.ID)
	attemptCode(t, <-result, "unauthorized")
}

func TestRunSelectionIdleReadDoesNotWaitForWriterAdmission(t *testing.T) {
	f := selectionFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.store.BackgroundWrite(t.Context(), func(context.Context, *sql.Tx) error { close(entered); <-release; return nil })
	}()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	worked, err := f.e.runNext(ctx, map[string]TargetReader{"test-capability": &refreshTestReader{}})
	close(release)
	writerErr := <-done
	if err != nil || worked || ctx.Err() != nil || writerErr != nil {
		t.Fatal("idle selector needed occupied writer admission", worked, err, ctx.Err(), writerErr)
	}
}

func TestRunSelectionNativeCoverageAndAdditionalGrantRemainChannelLocal(t *testing.T) {
	for _, granted := range []bool{false, true} {
		t.Run(fmt.Sprint(granted), func(t *testing.T) {
			f := selectionFixture(t)
			a, evidence := roundCapture(t, f, 7, false)
			roundFinalize(t, f, a, evidence, 7, false)
			if granted {
				roundGrant(t, f, 2, "cli", 0, 0, 8, "new-additional")
			}
			cli, _ := selectionEnqueue(t, f, "cli-once", "cli", nil)
			selectionNext(t, f, &refreshTestReader{})
			selected := selectionRead(t, f, cli.ID)
			op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), selected.OperationID)
			want := int64(4)
			if granted {
				want = 2
			}
			if err != nil || op.PullNumber != want {
				t.Fatal("native coverage or explicit additional grant selected wrong front", op, err)
			}
			hosted, _ := selectionEnqueue(t, f, "hosted-once", "hosted", nil)
			selectionNext(t, f, &refreshTestReader{})
			other := selectionRead(t, f, hosted.ID)
			op, err = f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), other.OperationID)
			if err != nil || op.PullNumber != 2 || op.Channel != "hosted" {
				t.Fatal("CLI progression held or advanced Hosted", op, err)
			}
		})
	}
}
