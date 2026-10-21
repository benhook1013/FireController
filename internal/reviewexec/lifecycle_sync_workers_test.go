package reviewexec

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func lifecycleProject(t *testing.T, f *attemptFixture, id, cap string, repo int64) {
	t.Helper()
	if id != attemptProject && id != otherAttemptProject {
		admissionSQL(t, f, `INSERT INTO projects(id,name,repository,github_repository_id) VALUES(?,?,'owner/repo',?)`, id, id, repo)
	}
	zero := int64(0)
	if _, err := f.e.Configure(t.Context(), f.scope(t, id, f.admin, true), "lifecycle-config-"+id, Configuration{ExpectedRevision: &zero, CapabilityID: cap, Mode: "independent", Queue: []int64{2}, Channels: []string{"cli"}, Paused: true}); err != nil {
		t.Fatal(err)
	}
	p := projectionPull(2)
	w := f.request("POST", "/api/v1/projects/"+id+"/reviews/observations", f.admin, "lifecycle-initial-"+id, encoded(reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: repo, Source: "fixture", ObservedAt: "2026-01-01T00:00:00Z", Queue: []reviews.PullInput{p}}))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
}

type lifecycleBlockingReader struct {
	mu              sync.Mutex
	active, maximum int
	perProject      map[int64]int
	entered         chan int64
	blocked         map[int64]<-chan struct{}
}

func (r *lifecycleBlockingReader) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	r.mu.Lock()
	r.active++
	r.maximum = max(r.maximum, r.active)
	r.perProject[id]++
	r.mu.Unlock()
	select {
	case r.entered <- id:
	case <-ctx.Done():
		r.leave()
		return reviewprovider.Repository{}, ctx.Err()
	}
	if gate := r.blocked[id]; gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			r.leave()
			return reviewprovider.Repository{}, ctx.Err()
		}
	}
	return reviewprovider.Repository{ID: id, FullName: fmt.Sprintf("owner/repo%d", id), DefaultBranch: "main"}, nil
}
func (r *lifecycleBlockingReader) leave() { r.mu.Lock(); r.active--; r.mu.Unlock() }
func (r *lifecycleBlockingReader) Pull(_ context.Context, repo string, n int64) (reviewprovider.Pull, error) {
	r.leave()
	id, _ := strconv.ParseInt(strings.TrimPrefix(repo, "owner/repo"), 10, 64)
	p := lifecyclePull(n)
	p.RepositoryID = id
	return p, nil
}
func lifecycleWorkerStart(t *testing.T, f *attemptFixture, readers map[string]TargetReader) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done, err := f.e.StartLifecycleSync(ctx, readers, LifecycleSyncOptions{ScanInterval: 5 * time.Millisecond})
	if err != nil || done == nil {
		cancel()
		t.Fatal(done, err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("lifecycle worker leaked")
		}
	})
	return cancel, done
}

func TestLifecycleSyncWorkersFairAcrossProjectsAndSerializeAccount(t *testing.T) {
	f := attemptProjectionFixture(t, true)
	lifecycleProject(t, f, otherAttemptProject, "other-account", 802)
	lifecycleProject(t, f, "exec-project-three", "test-capability", 803)
	gate := make(chan struct{})
	r := &lifecycleBlockingReader{perProject: map[int64]int{}, entered: make(chan int64, 16), blocked: map[int64]<-chan struct{}{801: gate, 803: gate}}
	cancel, done := lifecycleWorkerStart(t, f, map[string]TargetReader{"test-capability": r, "other-account": r})
	seen := map[int64]bool{}
	for len(seen) < 2 {
		select {
		case id := <-r.entered:
			seen[id] = true
		case <-time.After(2 * time.Second):
			t.Fatal("independent account did not progress", seen)
		}
	}
	if !seen[801] || !seen[802] || seen[803] {
		t.Fatal("same account took another slot", seen)
	}
	select {
	case id := <-r.entered:
		t.Fatal("duplicate tick or same-account read", id)
	case <-time.After(30 * time.Millisecond):
	}
	if duplicate, err := f.e.StartLifecycleSync(t.Context(), map[string]TargetReader{"test-capability": r}); err == nil || duplicate != nil {
		t.Fatal("duplicate owner")
	}
	cancel()
	refreshStop(t, cancel, done)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != 0 || r.maximum > 4 || r.perProject[801] != 1 || r.perProject[802] != 1 || r.perProject[803] != 0 {
		t.Fatal("worker ownership or coalescing", r.active, r.maximum, r.perProject)
	}
}

func TestLifecycleSyncWorkersFourSlotsAndCancellationJoins(t *testing.T) {
	f := attemptProjectionFixture(t, true)
	gate := make(chan struct{})
	r := &lifecycleBlockingReader{perProject: map[int64]int{}, entered: make(chan int64, 16), blocked: map[int64]<-chan struct{}{}}
	readers := map[string]TargetReader{"test-capability": r, "other-account": r}
	r.blocked[801] = gate
	for i := range 4 {
		id := fmt.Sprintf("lifecycle-project-%d", i)
		cap := fmt.Sprintf("lifecycle-cap-%d", i)
		repo := int64(810 + i)
		f.e.capabilities[cap] = Capability{ID: cap, Provider: "coderabbit", Account: cap, Channels: []string{"cli"}}
		readers[cap] = r
		r.blocked[repo] = gate
		lifecycleProject(t, f, id, cap, repo)
	}
	cancel, done := lifecycleWorkerStart(t, f, readers)
	for range 4 {
		select {
		case <-r.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("four slots did not start")
		}
	}
	select {
	case id := <-r.entered:
		t.Fatal("fifth provider read started", id)
	case <-time.After(30 * time.Millisecond):
	}
	refreshStop(t, cancel, done)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != 0 || r.maximum != 4 {
		t.Fatal("unjoined or unbounded", r.active, r.maximum)
	}
}

func TestLifecycleSyncDisabledProjectionAndNoEnrollmentStayLocal(t *testing.T) {
	f := newAttemptFixture(t)
	reader := &refreshTestReader{}
	if done, err := f.e.StartLifecycleSync(t.Context(), map[string]TargetReader{"test-capability": reader}); done != nil || err != nil || reader.calls.Load() != 0 {
		t.Fatal("disabled projection started polling", done, err)
	}
	f = attemptProjectionFixture(t, false)
	cancel, done := lifecycleWorkerStart(t, f, map[string]TargetReader{"test-capability": reader})
	refreshStop(t, cancel, done)
	if reader.calls.Load() != 0 {
		t.Fatal("unenrolled target fetched")
	}
}
