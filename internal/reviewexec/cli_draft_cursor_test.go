package reviewexec

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestCLIDraftProbeSelectionRotatesBeforeClaim(t *testing.T) {
	now := time.Now()
	e := &Engine{schedulingNow: func() time.Time { return now }}
	operations := []Operation{
		{ID: "review_operation_c", Channel: "cli", Reason: cliDraftWait},
		{ID: "review_operation_a", Channel: "cli", Reason: cliDraftWait},
		{ID: "review_operation_b", Channel: "cli", Reason: cliDraftWait},
		{ID: "review_operation_0", Channel: "hosted", Reason: cliDraftWait},
		{ID: "review_operation_1", Channel: "cli"},
	}
	for _, want := range []string{"review_operation_a", "review_operation_b", "review_operation_c", "review_operation_a"} {
		if got := e.nextDraftProbe(operations); got != want {
			t.Fatalf("selection without claim did not rotate: got %q, want %q", got, want)
		}
		if len(e.draftProbeNext) != 0 {
			t.Fatal("selection acquired a read throttle")
		}
	}
	e.draftProbeNext = map[string]time.Time{"review_operation_b": now.Add(cliDraftProbeInterval)}
	if got := e.nextDraftProbe(operations); got != "review_operation_c" {
		t.Fatal("selection ignored an existing throttle", got)
	}
	cursor := e.draftProbeCursor
	if got := e.nextDraftProbe(nil); got != "" || e.draftProbeCursor != cursor {
		t.Fatal("empty selection changed rotation", got, e.draftProbeCursor)
	}
}

func TestCLIDraftProbeDamagedWaitDoesNotStarveOtherProject(t *testing.T) {
	for _, damage := range []string{"revision gap", "malformed history"} {
		t.Run(damage, func(t *testing.T) {
			f := supervisorFixture(t)
			f.configure(t, otherAttemptProject, "test-capability", 0, false)
			first := f.enqueue(t, attemptProject, "draft-cursor-first", "cli", 2, 1)
			second := f.enqueue(t, otherAttemptProject, "draft-cursor-second", "cli", 2, 1)
			for _, owner := range [][2]string{{attemptProject, first.ID}, {otherAttemptProject, second.ID}} {
				o, err := readOperation(t.Context(), f.store.DB(), owner[0], owner[1])
				if err != nil {
					t.Fatal(err)
				}
				if err := f.e.recordCLIDraftDecision(t.Context(), o, cliDraftWait); err != nil {
					t.Fatal(err)
				}
			}
			badID, healthyID, badProject := first.ID, second.ID, attemptProject
			if second.ID < first.ID {
				badID, healthyID, badProject = second.ID, first.ID, otherAttemptProject
			}
			for revision := int64(1); revision <= 2; revision++ {
				c := Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4, 6}, Channels: []string{"cli", "hosted"}}
				if revision == 2 {
					c.Queue = append(c.Queue, 8)
				}
				if _, err := f.e.Configure(t.Context(), f.scope(t, badProject, f.admin, true), fmt.Sprintf("cursor-suffix-%d", revision), c); err != nil {
					t.Fatal(err)
				}
			}
			if damage == "revision gap" {
				admissionSQL(t, f, `DELETE FROM review_execution_config WHERE project_id=? AND revision=2`, badProject)
			} else {
				admissionSQL(t, f, `UPDATE review_execution_config SET body_json='{' WHERE project_id=? AND revision=2`, badProject)
			}
			reader := &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
				return reviewprovider.Repository{}, errors.New("source unavailable")
			}}
			driver := &draftWaitDriver{reader: reader}
			drivers := map[string]Driver{"test-capability": driver}
			tables := []string{"review_operations", "review_execution_config", "review_execution_attempts", "review_operation_waits", "review_execution_transitions", "review_execution_reservations", "events", "receipts"}
			before := make(map[string]string)
			for _, table := range tables {
				before[table] = snapshotTable(t, f, table)
			}
			if err := f.e.processNext(t.Context(), drivers); err != nil {
				t.Fatal("damaged first wait stopped the pass", err)
			}
			if reader.calls.Load() != 0 || draftProbeClaimed(f.e, badID) {
				t.Fatal("damaged wait reached a provider probe")
			}
			if err := f.e.processNext(t.Context(), drivers); err != nil {
				t.Fatal("later healthy wait stopped the pass", err)
			}
			if reader.calls.Load() != 1 || !draftProbeClaimed(f.e, healthyID) || draftProbeClaimed(f.e, badID) {
				t.Fatal("pre-claim damage starved another project's Draft probe", reader.calls.Load())
			}
			if driver.prepares.Load() != 0 || driver.calls.Load() != 0 {
				t.Fatal("Draft selection authorized preparation or dispatch")
			}
			for _, table := range tables {
				if before[table] != snapshotTable(t, f, table) {
					t.Fatal("probe rotation changed retained history", table)
				}
			}
		})
	}
}

func draftProbeClaimed(e *Engine, operationID string) bool {
	e.draftProbeMu.Lock()
	defer e.draftProbeMu.Unlock()
	_, claimed := e.draftProbeNext[operationID]
	return claimed
}

func TestCLIDraftProbePassIsBoundedAndRotatesAfterTimeout(t *testing.T) {
	f := supervisorFixture(t)
	seed := f.enqueue(t, attemptProject, "rotating-draft-probes", "cli", 2, 1)
	for i := range 2 {
		schedulerQueuedCopy(t, f, seed.ID, fmt.Sprintf("review_operation_rotate%03d", i))
	}
	admissionSQL(t, f, `UPDATE review_operations SET reason=?`, cliDraftWait)
	now := time.Now()
	f.e.schedulingNow = func() time.Time { return now }
	reader := &refreshTestReader{repository: func(ctx context.Context, _ int64) (reviewprovider.Repository, error) {
		short, cancel := context.WithTimeout(ctx, time.Millisecond)
		defer cancel()
		<-short.Done()
		return reviewprovider.Repository{}, short.Err()
	}}
	d := &draftWaitDriver{reader: reader}
	seen := map[string]bool{}
	for pass := range 3 {
		before := reader.calls.Load()
		if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": d}); err != nil {
			t.Fatal(err)
		}
		if reader.calls.Load()-before != 1 {
			t.Fatal("one scheduling pass performed multiple Draft probes", pass, reader.calls.Load()-before)
		}
		f.e.draftProbeMu.Lock()
		for id, next := range f.e.draftProbeNext {
			if next.After(now) {
				if seen[id] {
					t.Error("expired early probe starved a later wait", id)
				}
				seen[id] = true
			}
		}
		f.e.draftProbeMu.Unlock()
		now = now.Add(cliDraftProbeInterval)
	}
	if len(seen) != 3 || d.prepares.Load() != 0 || d.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
		t.Fatal("probe rotation skipped a wait or acquired review capacity", seen)
	}
}

func TestCLIDraftProbeThrottleOnlyBlocksWaitingRequests(t *testing.T) {
	for _, cause := range []string{"recent", "capacity"} {
		for _, waiting := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/waiting=%t", cause, waiting), func(t *testing.T) {
				f := supervisorFixture(t)
				now := time.Now()
				f.e.schedulingNow = func() time.Time { return now }
				request := f.enqueue(t, attemptProject, "probe-throttled", "cli", 2, 1)
				if waiting {
					o, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), request.ID)
					if err != nil {
						t.Fatal(err)
					}
					if err := f.e.recordCLIDraftDecision(t.Context(), o, cliDraftWait); err != nil {
						t.Fatal(err)
					}
				}
				f.e.draftProbeNext = map[string]time.Time{}
				if cause == "recent" {
					f.e.draftProbeNext[request.ID] = now.Add(cliDraftProbeInterval)
				} else {
					for i := range cliDraftProbeCapacity {
						f.e.draftProbeNext[fmt.Sprintf("other-%d", i)] = now.Add(cliDraftProbeInterval)
					}
				}
				d := &draftWaitDriver{reader: &refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) {
					t.Error("throttled probe still read the provider")
					return reviewprovider.Pull{}, nil
				}}}
				if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": d}); err != nil {
					t.Fatal(err)
				}
				o, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), request.ID)
				if err != nil {
					t.Fatal(err)
				}
				if waiting {
					if o.State != "queued" || o.Reason != cliDraftWait || o.AttemptID != "" || d.prepares.Load() != 0 {
						t.Fatal("waiting request bypassed probe throttle", o)
					}
				} else if o.State != "completed" || d.prepares.Load() != 1 || d.calls.Load() != 1 {
					t.Fatal("ordinary request was delayed by draft probe throttle", o, d.prepares.Load(), d.calls.Load())
				}
			})
		}
	}
}

func schedulerQueuedCopy(t *testing.T, f *attemptFixture, seed, id string) {
	t.Helper()
	if _, err := f.store.DB().Exec(`INSERT INTO review_operations (id,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at,capability_fingerprint,policy_revision) SELECT ?,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,'queued',1,'','',created_at,updated_at,capability_fingerprint,policy_revision FROM review_operations WHERE id=?`, id, seed); err != nil {
		t.Fatal(err)
	}
}
