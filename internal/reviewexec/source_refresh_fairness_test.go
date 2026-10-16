package reviewexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type refreshFairReader struct {
	mu                                 sync.Mutex
	active, maximum                    int
	byProject, maximumByProject, calls map[int64]int
	blocked                            map[int64]<-chan struct{}
	entered                            chan int64
}

func newRefreshFairReader(blocked map[int64]<-chan struct{}) *refreshFairReader {
	return &refreshFairReader{byProject: map[int64]int{}, maximumByProject: map[int64]int{}, calls: map[int64]int{}, blocked: blocked, entered: make(chan int64, 128)}
}
func (r *refreshFairReader) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	r.mu.Lock()
	r.active++
	r.maximum = max(r.maximum, r.active)
	r.byProject[id]++
	r.maximumByProject[id] = max(r.maximumByProject[id], r.byProject[id])
	r.calls[id]++
	r.mu.Unlock()
	r.entered <- id
	if gate := r.blocked[id]; gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			r.finish(id)
			return reviewprovider.Repository{}, ctx.Err()
		}
	}
	return reviewprovider.Repository{ID: id, FullName: fmt.Sprintf("owner/repo%d", id), DefaultBranch: "main"}, nil
}
func (r *refreshFairReader) finish(id int64) {
	r.mu.Lock()
	r.active--
	r.byProject[id]--
	r.mu.Unlock()
}
func (r *refreshFairReader) Pull(_ context.Context, repository string, number int64) (reviewprovider.Pull, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(repository, "owner/repo"), 10, 64)
	if err != nil {
		return reviewprovider.Pull{}, err
	}
	r.finish(id)
	return reviewprovider.Pull{Number: number, RepositoryID: id, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
}
func refreshFairConfigure(t *testing.T, f *attemptFixture, project string, count int) {
	t.Helper()
	queue := make([]int64, count)
	for i := range queue {
		queue[i] = int64(i + 1)
	}
	zero := int64(0)
	_, err := f.e.Configure(t.Context(), f.scope(t, project, f.admin, true), "fair-config-"+project, Configuration{ExpectedRevision: &zero, CapabilityID: "test-capability", Mode: "independent", Queue: queue, Channels: []string{"cli", "hosted"}})
	if err != nil {
		t.Fatal(err)
	}
}
func refreshFairEnqueue(t *testing.T, f *attemptFixture, scope core.Scope, number int64) Receipt {
	t.Helper()
	raw, err := f.e.EnqueueSourceRefresh(t.Context(), scope, fmt.Sprintf("fair-%s-%d", scope.ProjectID, number), SourceRefreshRequest{ExpectedConfigurationRevision: 1, PullNumber: number, HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if json.Unmarshal(raw, &receipt) != nil {
		t.Fatal("receipt")
	}
	return receipt
}

func TestSourceRefreshFairnessBlockedProjectDoesNotOwnPool(t *testing.T) {
	f := newAttemptFixture(t)
	refreshFairConfigure(t, f, attemptProject, 16)
	refreshFairConfigure(t, f, otherAttemptProject, 1)
	aScope := f.scope(t, attemptProject, f.writer, false)
	bScope := f.scope(t, otherAttemptProject, f.other, false)
	var backlog []Receipt
	for n := int64(1); n <= 16; n++ {
		backlog = append(backlog, refreshFairEnqueue(t, f, aScope, n))
	}
	blocked := make(chan struct{})
	reader := newRefreshFairReader(map[int64]<-chan struct{}{801: blocked})
	cancel, done := refreshStart(t, f, reader)
	select {
	case id := <-reader.entered:
		if id != 801 {
			t.Fatal("oldest queued project not selected", id)
		}
	case <-time.After(time.Second):
		t.Fatal("first project did not start")
	}
	b := refreshFairEnqueue(t, f, bScope, 1)
	got, err := f.e.WaitSourceRefresh(t.Context(), bScope, b.ID, 2*time.Second)
	if err != nil || got.State != "completed" {
		t.Fatal("independent project waited behind blocked16-operation backlog", got.State, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes WHERE project_id=? AND state='running'`, attemptProject) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes WHERE project_id=? AND state='queued'`, attemptProject) != 15 {
		t.Fatal("one project occupied concurrent provider slots")
	}
	close(blocked)
	for _, r := range backlog {
		got, err = f.e.WaitSourceRefresh(t.Context(), aScope, r.ID, 3*time.Second)
		if err != nil || got.State != "completed" {
			t.Fatal("backlog failed to complete", got.State, err)
		}
	}
	refreshStop(t, cancel, done)
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.maximum > 4 || reader.maximumByProject[801] != 1 || reader.maximumByProject[802] != 1 || reader.active != 0 || reader.calls[801] != 16 || reader.calls[802] != 1 {
		t.Fatal("provider concurrency or duplicate-read invariant", reader.maximum, reader.maximumByProject, reader.calls, reader.active)
	}
	for _, id := range append(backlog, b) {
		if attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.source_refresh_updated' AND resource_id=? AND json_extract(payload_json,'$.state')='completed'`, id.ID) != 1 {
			t.Fatal("duplicate/missing completion", id.ID)
		}
	}
}

func refreshFairProject(t *testing.T, f *attemptFixture, project string, repositoryID int64, count int) core.Scope {
	t.Helper()
	admissionSQL(t, f, `INSERT INTO projects(id,name,github_repository_id,repository) VALUES(?,?,?,?)`, project, project, repositoryID, "owner/repo")
	refreshFairConfigure(t, f, project, count)
	return f.scope(t, project, f.admin, true)
}

func TestSourceRefreshFairnessFourSlotsAndCancellationJoinAll(t *testing.T) {
	f := newAttemptFixture(t)
	release := make(chan struct{})
	blocked := map[int64]<-chan struct{}{}
	var receipts []Receipt
	var scopes []core.Scope
	for i := range 5 {
		id := int64(901 + i)
		scope := refreshFairProject(t, f, fmt.Sprintf("fair-pool-%d", i), id, 1)
		scopes = append(scopes, scope)
		receipts = append(receipts, refreshFairEnqueue(t, f, scope, 1))
		blocked[id] = release
	}
	reader := newRefreshFairReader(blocked)
	cancel, done := refreshStart(t, f, reader)
	for range 4 {
		select {
		case <-reader.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("four independent project slots did not start")
		}
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes WHERE state='running'`) != 4 || attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes WHERE state='queued'`) != 1 {
		t.Fatal("global source pool did not retain bounded four slots")
	}
	refreshStop(t, cancel, done)
	reader.mu.Lock()
	if reader.active != 0 || reader.maximum != 4 {
		t.Error("done closed before provider siblings joined", reader.active, reader.maximum)
	}
	for _, maximum := range reader.maximumByProject {
		if maximum != 1 {
			t.Error("same project concurrent provider reads", maximum)
		}
	}
	reader.mu.Unlock()
	if f.e.sourceRunning.Load() {
		t.Fatal("cancelled pool retained lifecycle ownership")
	}
	// Synchronous recovery occurs once before the next pool, preserving each ID.
	next := newRefreshFairReader(nil)
	cancel, done = refreshStart(t, f, next)
	for i, receipt := range receipts {
		got, err := f.e.WaitSourceRefresh(t.Context(), scopes[i], receipt.ID, 3*time.Second)
		if err != nil || got.State != "completed" {
			t.Fatal("cancel/restart lost source operation", got.State, err)
		}
		if attemptCount(t, f.store, `SELECT count(*) FROM events WHERE resource_id=? AND event_type='review.source_refresh_updated' AND json_extract(payload_json,'$.state')='completed'`, receipt.ID) != 1 {
			t.Fatal("restart duplicated completion", receipt.ID)
		}
	}
	refreshStop(t, cancel, done)
	if attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.source_refresh_updated' AND json_extract(payload_json,'$.reason')='source_read_interrupted'`) != 4 {
		t.Fatal("interrupted recovery did not run exactly once")
	}
}

func TestSourceRefreshFairnessFatalClaimCancelsAndJoinsProviderSiblings(t *testing.T) {
	f := newAttemptFixture(t)
	release := make(chan struct{})
	blocked := map[int64]<-chan struct{}{}
	for i := range 3 {
		id := int64(911 + i)
		scope := refreshFairProject(t, f, fmt.Sprintf("fair-fatal-%d", i), id, 1)
		refreshFairEnqueue(t, f, scope, 1)
		blocked[id] = release
	}
	faultScope := refreshFairProject(t, f, "fair-fatal-last", 914, 1)
	reader := newRefreshFairReader(blocked)
	_, done := refreshStart(t, f, reader)
	for range 3 {
		select {
		case <-reader.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("sibling providers did not start")
		}
	}
	admissionSQL(t, f, `CREATE TRIGGER source_fair_claim_fault BEFORE UPDATE ON review_source_refreshes WHEN NEW.project_id='fair-fatal-last' AND NEW.state='running' BEGIN SELECT RAISE(ABORT,'controlled claim fault'); END`)
	fault := refreshFairEnqueue(t, f, faultScope, 1)
	select {
	case err := <-done:
		attemptCode(t, err, "source_refresh_persistence_failed")
	case <-time.After(3 * time.Second):
		t.Fatal("fatal source claim did not cancel provider siblings")
	}
	if _, ok := <-done; ok {
		t.Fatal("pool done not closed")
	}
	reader.mu.Lock()
	active := reader.active
	reader.mu.Unlock()
	if active != 0 || f.e.sourceRunning.Load() || attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes WHERE state='running'`) != 3 {
		t.Fatal("fatal pool did not join/preserve interrupted reads", active)
	}
	current, err := f.e.ReadSourceRefresh(t.Context(), faultScope, fault.ID)
	if err != nil || current.State != "queued" || current.Revision != 1 {
		t.Fatal("failed claim fabricated advance", current, err)
	}
}

func TestSourceRefreshFairnessCyclesProjectsAndOldestWithinEach(t *testing.T) {
	f := newAttemptFixture(t)
	projects := []string{"fair-cycle-a", "fair-cycle-b", "fair-cycle-c"}
	oldest := map[string][]string{}
	for i, project := range projects {
		scope := refreshFairProject(t, f, project, int64(921+i), 2)
		refreshFairEnqueue(t, f, scope, 2)
		refreshFairEnqueue(t, f, scope, 1)
		oldest[project] = refreshFairQueuedOrder(t, f, project)
	}
	for _, held := range []bool{false, true} {
		// A fresh fixture is unnecessary: completed first-pass rows are terminal;
		// new keys plus fresh heads create the second pass without editing history.
		if held {
			for _, project := range projects {
				scope := f.scope(t, project, f.admin, true)
				for n := int64(1); n <= 2; n++ {
					raw, err := f.e.EnqueueSourceRefresh(t.Context(), scope, fmt.Sprintf("held-%s-%d", project, n), SourceRefreshRequest{ExpectedConfigurationRevision: 1, PullNumber: n, HeadSHA: strings.Repeat("b", 40)})
					if err != nil {
						t.Fatal(err)
					}
					if !json.Valid(raw) {
						t.Fatal("receipt")
					}
				}
				oldest[project] = refreshFairQueuedOrder(t, f, project)
			}
		}
		reader := newRefreshFairReader(nil)
		readers := map[string]TargetReader{"test-capability": reader}
		if held {
			readers = nil
		}
		schedule := newSourceRefreshSchedule()
		for i := range 6 {
			project := projects[i%3]
			worked, err := f.e.refreshNextScheduled(t.Context(), readers, schedule)
			if err != nil || !worked || schedule.lastProject != project {
				t.Fatal("source cursor did not cycle eligible projects", i, schedule.lastProject, err)
			}
			var state string
			if err := f.store.DB().QueryRow(`SELECT state FROM review_source_refreshes WHERE id=?`, oldest[project][i/3]).Scan(&state); err != nil || state != map[bool]string{false: "completed", true: "held"}[held] {
				t.Fatal("source cursor skipped oldest row within project", i, state, err)
			}
		}
		if worked, err := f.e.refreshNextScheduled(t.Context(), readers, schedule); err != nil || worked || schedule.lastProject != projects[2] {
			t.Fatal("idle pass advanced source cursor or entered writer", worked, err)
		}
	}
}

func TestSourceRefreshFairnessClaimGuardCancellation(t *testing.T) {
	f := newAttemptFixture(t)
	schedule := newSourceRefreshSchedule()
	schedule.claim <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if worked, err := f.e.refreshNextScheduled(ctx, nil, schedule); worked || !errors.Is(err, context.Canceled) {
		t.Fatal("claim guard ignored cancellation", worked, err)
	}
}

func refreshFairQueuedOrder(t *testing.T, f *attemptFixture, project string) []string {
	t.Helper()
	rows, err := f.store.DB().Query(`SELECT id FROM review_source_refreshes WHERE project_id=? AND state='queued' ORDER BY created_at,id`, project)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestSourceRefreshFairnessSelectionUsesActiveIndexes(t *testing.T) {
	f := newAttemptFixture(t)
	for _, query := range []struct {
		name, sql string
		args      []any
	}{
		{"selection", sourceRefreshSelection, []any{""}},
		{"claim-recheck", sourceRefreshProjectBusy, []any{attemptProject}},
	} {
		t.Run(query.name, func(t *testing.T) {
			rows, err := f.store.DB().Query("EXPLAIN QUERY PLAN "+query.sql, query.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			t.Log(strings.Join(plan, "; "))
			bounded := false
			queuedBounded := query.name != "selection"
			for _, detail := range plan {
				if strings.Contains(detail, "review_source_refresh_project_pending") || strings.Contains(detail, "review_source_refresh_pending (state=?") {
					if query.name == "claim-recheck" || strings.Contains(detail, "busy ") {
						bounded = true
					}
					if strings.Contains(detail, " q ") {
						queuedBounded = true
					}
				}
			}
			if !bounded || !queuedBounded {
				t.Fatal("source selection/claim can walk retained history", plan)
			}
		})
	}
}
