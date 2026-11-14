package reviewexec

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func selectionHistoryFixture(t *testing.T, queue []int64, merged int64) *attemptFixture {
	t.Helper()
	f := schedulingFixtureWithQueue(t, queue)
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	var err error
	f.e, err = New(f.store, attemptCapabilities, WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, f.store)
	f.handler = mux
	pulls := []reviews.PullInput{}
	for _, n := range queue {
		p := projectionPull(n)
		if n <= merged {
			p.State = "merged"
		}
		pulls = append(pulls, p)
	}
	request := reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: 801, Source: "retained-history", ObservedAt: "2026-01-01T00:00:00Z", Queue: pulls}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "initial-history", encoded(request))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	return f
}

func TestRunSelectionKnownMergedPrefixAndInterleavedPausedDonorsUseNoCalls(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			queue := make([]int64, 86)
			for i := range queue {
				queue[i] = int64(i + 1)
			}
			f := selectionHistoryFixture(t, queue, 49)
			for _, n := range queue[57:85] {
				schedulingPause(t, f, n, 0, true)
			}
			receipt, _ := selectionEnqueue(t, f, "long-history", channel, nil)
			r := selectionRead(t, f, receipt.ID)
			c, err := f.e.configurationRevision(t.Context(), r.ProjectID, r.ConfigurationRevision)
			if err != nil {
				t.Fatal(err)
			}
			var pulls, active atomic.Int32
			var mu sync.Mutex
			identities := map[int64]int{}
			assertReads := func() {
				t.Helper()
				if active.Load() != 0 {
					t.Fatal("metadata readers not joined", active.Load())
				}
				mu.Lock()
				defer mu.Unlock()
				// Known-live metadata is read exactly once per ordered phase;
				// cancelled speculative suffix reads must not amplify pressure.
				for n := int64(50); n <= 57; n++ {
					maximum := 1
					if count := identities[n]; count < 1 || count > maximum {
						t.Errorf("PR%d metadata reads = %d, want 1 to %d", n, count, maximum)
					}
				}
				if len(identities) != 9 || identities[86] != 1 {
					t.Error("unexpected source identities", identities)
				}
			}
			reader := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
				pulls.Add(1)
				active.Add(1)
				defer active.Add(-1)
				mu.Lock()
				identities[n]++
				mu.Unlock()
				if n <= 49 || n >= 58 && n <= 85 {
					t.Errorf("fetched irrelevant historical PR %d", n)
				}
				timer := time.NewTimer(10 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-ctx.Done():
					return reviewprovider.Pull{}, ctx.Err()
				}
				p, err := selectionSnapshotReader{}.Pull(ctx, repo, n)
				if n < 86 {
					p.IsDraft = new(true)
				}
				return p, err
			}}
			choice, err := f.e.chooseRunWithSourceBudget(t.Context(), r, 801, c, reader, 500*time.Millisecond)
			if err != nil || choice.state != "selected" || choice.number != 86 || len(choice.prefix) != 86 || reader.calls.Load() != int64(pulls.Load())+1 {
				t.Fatal(choice.state, choice.reason, choice.number, len(choice.prefix), pulls.Load(), reader.calls.Load(), err)
			}
			assertReads()
			if choice.prefix[0].ObservedAt != "2026-01-01T00:00:00Z" || !choice.prefix[0].retainedMerged || !choice.prefix[57].schedulingPaused || choice.prefix[57].ObservedAt != "" {
				t.Fatal("invented fresh history", choice.prefix[0], choice.prefix[57])
			}
			identities = map[int64]int{}
			pulls.Store(0)
			reader.calls.Store(0)
			selectionNext(t, f, reader)
			got := selectionRead(t, f, receipt.ID)
			frame, found, err := readFrontFrame(t.Context(), f.store.DB(), attemptProject, channel)
			if err != nil || !found || got.State != "selected" || frame.complete || len(frame.prefix) != 86 || reader.calls.Load() != int64(pulls.Load())+1 {
				t.Fatal(got, frame.complete, len(frame.prefix), reader.calls.Load(), err)
			}
			assertReads()
			if err := ValidateFrontSnapshot(t.Context(), f.store.DB()); err != nil {
				t.Fatal(err)
			}
			current, err := f.e.configurationRevision(t.Context(), r.ProjectID, r.ConfigurationRevision)
			if err != nil || !slices.Equal(current.Queue, queue) {
				t.Fatal("history removed from queue", current, err)
			}
			front := frontChannel(t, frontRead(t, f), channel)
			if front.State != "ready" || front.PullNumber != 86 {
				t.Fatal("skipped history hid actual front", front)
			}
			t.Logf("86 queued PRs: 49 merged + 28 paused read zero times; 9 live identities with bounded lookahead + repository = %d provider calls", reader.calls.Load())
		})
	}
}

func TestRunSelectionMergedFramesReusedAcrossChannels(t *testing.T) {
	f := selectionFixtureWithQueue(t, []int64{1, 2})
	first, _ := selectionEnqueue(t, f, "learn-merged", "cli", nil)
	reader := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
		return selectionSnapshotReader{merged: n == 1}.Pull(ctx, repo, n)
	}}
	selectionNext(t, f, reader)
	if selectionRead(t, f, first.ID).State != "selected" || reader.calls.Load() != 3 {
		t.Fatal(reader.calls.Load())
	}
	// A fresh engine has no in-memory source cache. The durable terminal fact is
	// reusable by the other channel while its live target is still fetched.
	var err error
	f.e, err = New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	reader.calls.Store(0)
	reader.pull = func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
		if n == 1 {
			t.Error("refetched durable merged history")
		}
		return selectionSnapshotReader{}.Pull(ctx, repo, n)
	}
	next, _ := selectionEnqueue(t, f, "reuse-merged", "hosted", nil)
	selectionNext(t, f, reader)
	if selectionRead(t, f, next.ID).State != "selected" || reader.calls.Load() != 2 {
		t.Fatal(selectionRead(t, f, next.ID), reader.calls.Load())
	}
}

func TestRunSelectionRetainedHistoryInvalidationRejectsConcurrentChanges(t *testing.T) {
	for _, change := range []string{"reopen-correction", "head-correction", "retirement", "unpause"} {
		t.Run(change, func(t *testing.T) {
			f := selectionHistoryFixture(t, []int64{1, 2, 3}, 1)
			schedulingPause(t, f, 2, 0, true)
			receipt, _ := selectionEnqueue(t, f, "change-history", "cli", nil)
			reader := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
				if n != 3 {
					t.Errorf("unexpected read %d", n)
				}
				switch change {
				case "reopen-correction":
					admissionSQL(t, f, `UPDATE review_pull_versions SET state='open' WHERE project_id=? AND number=1`, attemptProject)
				case "head-correction":
					admissionSQL(t, f, `UPDATE review_pull_versions SET head_sha=? WHERE project_id=? AND number=1`, strings.Repeat("c", 40), attemptProject)
				case "retirement":
					admissionSQL(t, f, `DELETE FROM review_current_pulls WHERE project_id=? AND number=1`, attemptProject)
				case "unpause":
					schedulingPause(t, f, 2, 1, false)
				}
				return selectionSnapshotReader{}.Pull(ctx, repo, n)
			}}
			selectionNext(t, f, reader)
			got := selectionRead(t, f, receipt.ID)
			if got.State != "held" || got.Reason != "selection_changed" || got.OperationID != "" || reader.calls.Load() != 2 || attemptCount(t, f.store, `SELECT count(*) FROM review_front_frames`) != 0 {
				t.Fatal(got, reader.calls.Load())
			}
		})
	}
}

func TestRunSelectionPausedActualParentsStillAcquired(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, queue := range [][]int64{{2, 4}, {4, 2}} {
			t.Run(fmt.Sprintf("%s/parent-first-%t", channel, queue[0] == 2), func(t *testing.T) {
				f := schedulingFixtureWithQueue(t, queue)
				revision := int64(1)
				if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "stack", Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "stacked", Queue: queue, Channels: []string{"cli", "hosted"}}); err != nil {
					t.Fatal(err)
				}
				schedulingPause(t, f, 2, 0, true)
				pauseBefore, err := f.e.ReadPullScheduling(t.Context(), selectionScope(t, f, attemptProject, f.reader, "read"), 2)
				if err != nil {
					t.Fatal(err)
				}
				evidenceBefore := attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`)
				roundsBefore := attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions`)
				reader := &actualParentSelectionReader{pulls: map[int64]reviewprovider.Pull{
					2: {Number: 2, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), HeadRef: "parent", BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1},
					4: {Number: 4, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("c", 40), HeadRef: "child", BaseSHA: strings.Repeat("a", 40), BaseRef: "parent", ChangedFiles: 1},
				}}
				receipt, _ := selectionEnqueue(t, f, "paused-parent", channel, nil)
				selectionNext(t, f, reader)
				got := selectionRead(t, f, receipt.ID)
				frame, found, err := readFrontFrame(t.Context(), f.store.DB(), attemptProject, channel)
				if err != nil || !found || !frame.complete || len(frame.prefix) != 2 {
					t.Fatal(got, frame, err)
				}
				for _, p := range frame.prefix {
					if p.ObservedAt == "" || p.SourceReason == "pull_scheduling_paused" {
						t.Fatal("actual dependency skipped", p)
					}
				}
				if got.State != "selected" || got.OperationID == "" {
					t.Fatal(got)
				}
				child := roundOperation(t, f, got.OperationID)
				if child.PullNumber != 4 || child.HeadSHA != strings.Repeat("c", 40) || child.AllowOutOfOrder {
					t.Fatal("selected child lost exact source or bypassed ordinary guards", child)
				}
				pauseAfter, err := f.e.ReadPullScheduling(t.Context(), selectionScope(t, f, attemptProject, f.reader, "read"), 2)
				if err != nil || pauseAfter != pauseBefore || !pauseAfter.Paused || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != evidenceBefore || attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions`) != roundsBefore {
					t.Fatal("child selection changed ancestor hold or accounting", pauseAfter, err)
				}
				for _, plan := range frame.plans() {
					if plan.Pull.Number == 4 && (plan.ParentPullNumber != 2 || plan.ParentSHA != strings.Repeat("a", 40) || plan.Pull.BaseSHA != plan.ParentSHA) {
						t.Fatal("child lost exact paused-parent witness", plan)
					}
					if plan.Pull.Number == 2 {
						err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
							own, err := f.e.evaluateFrontPlan(ctx, tx, Run{ProjectID: attemptProject, Channel: channel}, 801, &plan)
							if err == nil && (own.state != "held" || own.reason != "pull_scheduling_paused") {
								t.Fatal("ancestor's own pause no longer holds", own)
							}
							return err
						})
						if err != nil {
							t.Fatal(err)
						}
					}
				}
			})
		}
	}
}

func TestRunSelectionRetainedMergedStillChecksSourceObligations(t *testing.T) {
	f := selectionHistoryFixture(t, []int64{2}, 2)
	integrationImportHold(t, f, 2)
	receipt, _ := selectionEnqueue(t, f, "merged-obligations", "cli", nil)
	reader := &refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) {
		t.Error("merged obligation caused provider refresh")
		return reviewprovider.Pull{}, nil
	}}
	selectionNext(t, f, reader)
	got := selectionRead(t, f, receipt.ID)
	if got.State != "held" || got.Reason != "source_obligations_pending" || reader.calls.Load() != 1 {
		t.Fatal(got, reader.calls.Load())
	}
}

func TestRunHistoryCurrentAllowsUnrelatedEvidence(t *testing.T) {
	f := selectionHistoryFixture(t, []int64{1}, 1)
	err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		history, err := readRunHistory(ctx, tx, attemptProject, 801, []int64{1})
		if err != nil {
			return err
		}
		plan := history[1]
		plan.ObservedAt = "2025-01-01T00:00:00Z"
		valid, err := runHistoryCurrent(ctx, tx, Run{ProjectID: attemptProject}, 801, Configuration{Queue: []int64{1}}, runChoice{prefix: []SourcePlan{plan}})
		if !valid {
			t.Error("observation time alone invalidated terminal lifecycle")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunSelectionMalformedOptionalFrameDoesNotBlockFreshSelection(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := selectionHistoryFixture(t, []int64{1, 2}, 1)
			frontSelect(t, f, "seed-frame", channel, &refreshTestReader{})
			admissionSQL(t, f, `UPDATE review_front_frames SET prefix_json='invalid' WHERE project_id=?`, attemptProject)
			receipt, _ := selectionEnqueue(t, f, "refresh-invalid-frame", "cli", nil)
			reader := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
				if n != 2 {
					t.Errorf("known merged row fetched: %d", n)
				}
				return selectionSnapshotReader{}.Pull(ctx, repo, n)
			}}
			selectionNext(t, f, reader)
			got := selectionRead(t, f, receipt.ID)
			if got.State != "selected" || reader.calls.Load() != 2 {
				t.Fatal(got, reader.calls.Load())
			}
		})
	}
}

func TestRunSelectionPausedFrontOmitsUnverifiedComparison(t *testing.T) {
	f := selectionHistoryFixture(t, []int64{1}, 0)
	schedulingPause(t, f, 1, 0, true)
	receipt, _ := selectionEnqueue(t, f, "all-paused", "cli", nil)
	reader := &refreshTestReader{}
	selectionNext(t, f, reader)
	got := frontChannel(t, frontRead(t, f), "cli")
	if selectionRead(t, f, receipt.ID).State != "held" || got.State != "held" || got.Reason != "pull_scheduling_paused" || got.PullNumber != 0 || got.HeadSHA != "" || got.BaseSHA != "" || got.SourceObservedAt != "" || reader.calls.Load() != 1 {
		t.Fatal(got, reader.calls.Load())
	}
	schedulingPause(t, f, 1, 1, false)
	got = frontChannel(t, frontRead(t, f), "cli")
	if got.State == "ready" || got.SourceObservedAt != "" {
		t.Fatal("unpause reused unacquired source", got)
	}
}

func TestRunSelectionSourceProgressDoesNotCountPausedMarkers(t *testing.T) {
	f := selectionHistoryFixture(t, []int64{1, 2, 3}, 1)
	schedulingPause(t, f, 2, 0, true)
	_, _ = selectionEnqueue(t, f, "source-failure-after-pause", "cli", nil)
	reader := &refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{}, errRunSourceBudget
	}}
	selectionNext(t, f, reader)
	got := frontChannel(t, frontRead(t, f), "cli")
	if got.State != "unknown" || got.SourceProgress == nil || *got.SourceProgress != (FrontSourceProgress{VerifiedPullCount: 1, QueuePullCount: 3, NextUnverifiedPullNumber: 2}) {
		t.Fatal(got)
	}
}

func TestRunSelectionPausedFrameCannotEraseMergedObservation(t *testing.T) {
	f := selectionFixtureWithQueue(t, []int64{1, 2})
	receipt, _ := selectionEnqueue(t, f, "learn-history-before-pause", "cli", nil)
	reader := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
		p, err := selectionSnapshotReader{merged: n == 1}.Pull(ctx, repo, n)
		if n == 2 {
			p.IsDraft = new(true)
		}
		return p, err
	}}
	selectionNext(t, f, reader)
	r := selectionRead(t, f, receipt.ID)
	c, err := f.e.configurationRevision(t.Context(), r.ProjectID, r.ConfigurationRevision)
	if err != nil {
		t.Fatal(err)
	}
	// A concurrently selected other channel may commit its older pause decision
	// after the terminal observation. It has no lifecycle facts to supersede it.
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		r.Channel = "hosted"
		return writeFrontFrame(ctx, tx, r, 801, c, runChoice{prefix: []SourcePlan{{Repository: reviewprovider.Repository{ID: 801}, Pull: reviewprovider.Pull{RepositoryID: 801, Number: 1}, SourceReason: "pull_scheduling_paused", schedulingPaused: true}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	reader.calls.Store(0)
	reader.pull = func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
		if n == 1 {
			t.Error("source-free pause erased merged observation")
		}
		return selectionSnapshotReader{}.Pull(ctx, repo, n)
	}
	next, _ := selectionEnqueue(t, f, "reuse-after-pause-frame", "hosted", nil)
	selectionNext(t, f, reader)
	if selectionRead(t, f, next.ID).State != "selected" || reader.calls.Load() != 2 {
		t.Fatal(selectionRead(t, f, next.ID), reader.calls.Load())
	}
}

func TestRunSelectionKnownLiveHintNeverReusesStaleSource(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := selectionHistoryFixture(t, []int64{1, 2, 3, 4}, 0)
			receipt, _ := selectionEnqueue(t, f, "known-live-fresh", channel, nil)
			var mu sync.Mutex
			seen := map[int64]int{}
			reader := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
				mu.Lock()
				seen[n]++
				mu.Unlock()
				p, err := selectionSnapshotReader{merged: n == 1}.Pull(ctx, repo, n)
				if n == 2 {
					p.HeadSHA = strings.Repeat("d", 40)
				}
				return p, err
			}}
			selectionNext(t, f, reader)
			got := selectionRead(t, f, receipt.ID)
			op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), got.OperationID)
			if err != nil || got.State != "selected" || op.PullNumber != 2 || op.HeadSHA != strings.Repeat("d", 40) {
				t.Fatal("performance hint replaced fresh source", got)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 2 || seen[1] != 1 || seen[2] != 1 {
				t.Fatal("known-live suffix fetched speculatively", seen)
			}
		})
	}
}
