package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"strings"
	"testing"
	"time"
)

func TestOfflineQueueCostBudgetComparison(t *testing.T) {
	// Before parent pipelining: 200ms completed 50 boundaries in 10.029s;
	// 400ms exhausted the unchanged 15s budget after 38 boundaries/prefix12.
	// Pipeline retains all 50 boundaries, overlapping only next raw metadata.
	for _, channel := range []string{"hosted", "cli"} {
		for _, adaptive := range []bool{false, true} {
			delay := 450 * time.Millisecond
			t.Run(fmt.Sprintf("%s/adaptive=%t", channel, adaptive), func(t *testing.T) {
				t.Parallel()
				queue := make([]int64, 17)
				pulls := map[int64]reviewprovider.Pull{}
				for i := range queue {
					n := int64(i + 1)
					queue[i] = n
					p := reviewprovider.Pull{Number: n, RepositoryID: 801, State: "open", HeadSHA: fmt.Sprintf("%040x", n), HeadRef: fmt.Sprintf("branch-%d", n), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1, IsDraft: new(false)}
					if n > 1 {
						p.BaseRef = fmt.Sprintf("branch-%d", n-1)
						p.BaseSHA = fmt.Sprintf("%040x", n-1)
					}
					pulls[n] = p
				}
				f := selectionFixtureWithQueue(t, queue)
				for n := int64(1); n < 17; n++ {
					frontClose(t, f, n, channel, fmt.Sprintf("close-%d", n))
				}
				revision := int64(1)
				_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "adaptive-stack-config", Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "stacked", Queue: queue, Channels: []string{"cli", "hosted"}})
				if err != nil {
					t.Fatal(err)
				}
				receipt, _ := selectionEnqueue(t, f, "adaptive-stack", channel, nil)
				r := selectionRead(t, f, receipt.ID)
				if adaptive {
					markFairAutomatic(t, f, r.ProjectID, r.ID)
				}
				c, err := f.e.configurationRevision(t.Context(), r.ProjectID, r.ConfigurationRevision)
				if err != nil {
					t.Fatal(err)
				}
				reader := &adaptiveStackReader{actualParentSelectionReader: actualParentSelectionReader{pulls: pulls}, delay: delay}
				budget := 15 * time.Second
				if adaptive {
					budget = selectionSourceAllowance(len(queue))
				}
				start := time.Now()
				var choice runChoice
				if adaptive {
					choice, err = f.e.chooseRun(t.Context(), r, 801, c, reader)
				} else {
					for retry := 0; retry < 3; retry++ {
						choice, err = f.e.chooseRunWithSourceBudget(t.Context(), r, 801, c, reader, budget)
						if err != nil || choice.reason != "source_budget_exhausted" {
							t.Fatal("fixed retry changed result", retry, choice.reason, err)
						}
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("17 real parent-linked open PRs delay=%s boundaries=%d prefix=%d state=%s reason=%s source=%s wall=%s", delay, reader.boundaries.Load(), len(choice.prefix), choice.state, choice.reason, choice.sourceElapsed, time.Since(start))
				if adaptive && (choice.state != "selected" || choice.number != 17 || len(choice.prefix) != 17 || reader.boundaries.Load() != 50 || choice.sourceElapsed >= budget) {
					t.Fatal(choice.state, choice.reason, len(choice.prefix), reader.boundaries.Load())
				}
				for _, plan := range choice.prefix {
					if plan.Pull.Number > 1 && (plan.ParentPullNumber != plan.Pull.Number-1 || plan.ParentSHA != fmt.Sprintf("%040x", plan.Pull.Number-1)) {
						t.Fatal("parent identity changed", plan)
					}
				}
				if !adaptive && (choice.reason != "source_budget_exhausted" || reader.boundaries.Load() != 147) {
					t.Fatal(choice.state, choice.reason)
				}
			})
		}
	}
}

func TestOfflineLongSourceBudgetDoesNotHoldDatabaseWriter(t *testing.T) {
	f := selectionFixture(t)
	receipt, _ := selectionEnqueue(t, f, "offline-long-source", "cli", nil)
	r := selectionRead(t, f, receipt.ID)
	c, err := f.e.configurationRevision(t.Context(), r.ProjectID, r.ConfigurationRevision)
	if err != nil {
		t.Fatal(err)
	}
	reader, entered, _ := selectionContentionReader()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.e.chooseRunWithSourceBudget(ctx, r, 801, c, reader, 45*time.Second); done <- err }()
	selectionContentionEntered(t, entered)
	start := time.Now()
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE projects SET name=name WHERE id=?`, attemptProject)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("isolated DB writer completed during blocked source in %s", time.Since(start))
	cancel()
	start = time.Now()
	select {
	case <-done:
		t.Logf("source cancellation joined in %s", time.Since(start))
	case <-time.After(time.Second):
		t.Fatal("source failed bounded cancellation")
	}
}

func TestFairSelectionContenderArrivesAfterBothSlotsBusy(t *testing.T) {
	f := selectionFixtureWithQueue(t, []int64{2})
	f.e.runSourceBudget = 45 * time.Second
	one, _ := selectionEnqueue(t, f, "slot-one", "hosted", nil)
	two, _ := selectionEnqueue(t, f, "slot-two", "cli", nil)
	markFairAutomatic(t, f, attemptProject, one.ID)
	markFairAutomatic(t, f, attemptProject, two.ID)
	entered := make(chan int64, 3)
	release := make(chan struct{}, 2)
	reader := &refreshTestReader{repository: func(ctx context.Context, id int64) (reviewprovider.Repository, error) {
		entered <- id
		if id == 801 {
			select {
			case <-release:
			case <-ctx.Done():
				return reviewprovider.Repository{}, ctx.Err()
			}
		}
		return reviewprovider.Repository{ID: id, FullName: "owner/repo", DefaultBranch: "main"}, nil
	}}
	cancel, done := selectionContentionStart(t, f, reader)
	for range 2 {
		select {
		case id := <-entered:
			if id != 801 {
				t.Fatalf("unexpected early contender %d", id)
			}
		case <-time.After(time.Second):
			t.Fatal("two project lanes did not enter")
		}
	}
	selectionCapacityProject(t, f, "contender-project", 900)
	raw, err := f.e.EnqueueRun(t.Context(), selectionScope(t, f, "contender-project", f.admin, "write"), "contender", RunRequest{Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err = json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	markFairAutomatic(t, f, "contender-project", receipt.ID)
	start := time.Now()
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE projects SET name=name WHERE id=?`, attemptProject)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("writer while BOTH selection slots occupied: %s", time.Since(start))
	select {
	case id := <-entered:
		t.Fatalf("unexpected third simultaneous slot %d", id)
	case <-time.After(100 * time.Millisecond):
	}
	t.Log("other-project source cannot start while both worker slots are occupied")
	release <- struct{}{}
	select {
	case id := <-entered:
		if id != 900 {
			t.Fatalf("wrong released contender %d", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("contender did not start after slot released")
	}
	selectionContentionStop(t, cancel, done)
}

func TestSelectionSourceAllowanceBounds(t *testing.T) {
	for _, c := range []struct {
		n    int
		want time.Duration
	}{{0, 15 * time.Second}, {1, 15 * time.Second}, {15, 15 * time.Second}, {17, 17 * time.Second}, {30, 30 * time.Second}, {100, 30 * time.Second}} {
		if got := selectionSourceAllowance(c.n); got != c.want {
			t.Fatal(c.n, got, c.want)
		}
	}
	b := runSourceBudget{remaining: 30 * time.Second}
	if err := b.read(t.Context(), func(ctx context.Context) error {
		d, ok := ctx.Deadline()
		if !ok || time.Until(d) > 15*time.Second {
			t.Fatal("individual request deadline expanded")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFairSelectionRecentProjectDoesNotReclaimBeforeContender(t *testing.T) {
	f := selectionFixtureWithQueue(t, []int64{2})
	old, _ := selectionEnqueue(t, f, "finished-prior-scan", "cli", nil)
	admissionSQL(t, f, `UPDATE review_selections SET state='held',reason='source_budget_exhausted',updated_at=? WHERE id=?`, stamp(), old.ID)
	waiting, _ := selectionEnqueue(t, f, "older-queued-retry", "cli", nil)
	selectionCapacityProject(t, f, "fair-contender", 900)
	raw, err := f.e.EnqueueRun(t.Context(), selectionScope(t, f, "fair-contender", f.admin, "write"), "newer-contender", RunRequest{Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err = json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	markFairAutomatic(t, f, attemptProject, waiting.ID)
	markFairAutomatic(t, f, "fair-contender", receipt.ID)
	// Simulate a previously accepted automatic intent requeued only after the
	// competing project arrived; created_at remains older, updated_at advances.
	admissionSQL(t, f, `UPDATE review_selections SET updated_at=? WHERE id=?`, time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano), waiting.ID)
	err = f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var project, id string
		var revision int64
		if err := nextFairSelection(ctx, tx).Scan(&project, &id, &revision); err != nil {
			return err
		}
		if project != "fair-contender" || id != receipt.ID {
			t.Fatalf("recent project reclaimed slot before waiting contender: %s %s (older own=%s)", project, id, waiting.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSelectionWallCeilingKeepsWorkerAliveAndNoAdmission(t *testing.T) {
	f := selectionFixture(t)
	receipt, _ := selectionEnqueue(t, f, "wall-bounded", "cli", nil)
	r := selectionRead(t, f, receipt.ID)
	c, err := f.e.configurationRevision(t.Context(), r.ProjectID, r.ConfigurationRevision)
	if err != nil {
		t.Fatal(err)
	}
	reader := &refreshTestReader{repository: func(ctx context.Context, id int64) (reviewprovider.Repository, error) {
		<-ctx.Done()
		return reviewprovider.Repository{}, ctx.Err()
	}}
	choice, err := f.e.chooseRunBounded(t.Context(), r, 801, c, reader, 20*time.Millisecond)
	if err != nil || choice.state != "held" || choice.reason != "selection_budget_exhausted" || choice.sourceReason != choice.reason || choice.number != 0 || t.Context().Err() != nil {
		t.Fatal(choice, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("wall timeout admitted operation")
	}
	if !transientAutomaticSourceHold(Run{Channel: "cli", State: "held", Reason: choice.reason, Revision: 3}) || transientAutomaticSourceHold(Run{Channel: "cli", State: "held", Reason: choice.reason, Revision: 9}) {
		t.Fatal("wall limit changed bounded retry cap")
	}
}

func markFairAutomatic(t *testing.T, f *attemptFixture, project, id string) {
	t.Helper()
	admissionSQL(t, f, `UPDATE grants SET can_manage=1 WHERE credential_id='exec-writer-cred'`)
	var configured bool
	if err := f.store.DB().QueryRow(`SELECT EXISTS(SELECT 1 FROM review_scheduling WHERE project_id=?)`, project).Scan(&configured); err != nil {
		t.Fatal(err)
	}
	if !configured {
		token := f.writer
		if project != attemptProject {
			token = f.admin
		}
		revision := int64(0)
		on := true
		if _, err := f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, project, token, "write"), "enable-fair-"+project, SchedulingRequest{ExpectedRevision: &revision, AutomaticHosted: &on, AutomaticCLI: &on}); err != nil {
			t.Fatal(err)
		}
	}
	admissionSQL(t, f, `INSERT INTO review_automatic_selections(selection_id,project_id,scheduling_revision,basis_sha256,actor_id,worker_id,credential_id)
 SELECT r.id,r.project_id,s.revision,?,r.actor_id,r.worker_id,s.credential_id FROM review_selections r JOIN review_scheduling s ON s.project_id=r.project_id WHERE r.project_id=? AND r.id=?`, fmt.Sprintf("%x", sha256.Sum256([]byte(id))), project, id)
}

func TestFairSelectionManualKeepsOriginalFIFO(t *testing.T) {
	f := selectionFixtureWithQueue(t, []int64{2})
	first, _ := selectionEnqueue(t, f, "manual-first", "cli", nil)
	selectionCapacityProject(t, f, "manual-contender", 900)
	raw, err := f.e.EnqueueRun(t.Context(), selectionScope(t, f, "manual-contender", f.admin, "write"), "automatic-second", RunRequest{Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	var second Receipt
	if err = json.Unmarshal(raw, &second); err != nil {
		t.Fatal(err)
	}
	markFairAutomatic(t, f, "manual-contender", second.ID)
	admissionSQL(t, f, `UPDATE review_selections SET updated_at=? WHERE id=?`, time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano), first.ID)
	err = f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var p, id string
		var rev int64
		if err := nextFairSelection(ctx, tx).Scan(&p, &id, &rev); err != nil {
			return err
		}
		if id != first.ID {
			t.Fatal("manual FIFO changed", id)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
