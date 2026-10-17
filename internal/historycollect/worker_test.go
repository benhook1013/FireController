package historycollect

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// The provider hook controls actual worker boundaries without sleeping through
// provider work or replacing the real SQLite claim/commit path.
type collectionWorkerProvider struct {
	*stepProvider
	reviewsPage func(context.Context, string, int64, int64) (reviewprovider.Page[reviewprovider.HostedReview], error)
}

func (p *collectionWorkerProvider) ReviewsPage(ctx context.Context, repo string, n, page int64) (reviewprovider.Page[reviewprovider.HostedReview], error) {
	if p.reviewsPage != nil {
		return p.reviewsPage(ctx, repo, n, page)
	}
	return p.stepProvider.ReviewsPage(ctx, repo, n, page)
}

func collectionWorkerSetup(t *testing.T, selection Selection) (*collectionStoreFixture, *Engine, core.Scope, Receipt) {
	t.Helper()
	f := newCollectionStoreFixture(t)
	project, worker := f.project(t, "selected/repository")
	scope := f.scope(t, project, worker, f.admin)
	e, err := New(f.store, f.provider, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	body, err := e.Enqueue(t.Context(), scope, "worker-collection", selection)
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if json.Unmarshal(body, &receipt) != nil {
		t.Fatal("invalid receipt")
	}
	return f, e, scope, receipt
}
func collectionAdvance(t *testing.T, e *Engine) {
	t.Helper()
	worked, err := e.advance(t.Context())
	if err != nil || !worked {
		t.Fatalf("advance worked=%t: %v", worked, err)
	}
}
func collectionWorkerRead(t *testing.T, e *Engine, scope core.Scope, id string) Operation {
	t.Helper()
	o, err := e.Read(t.Context(), scope, id)
	if err != nil {
		t.Fatal(err)
	}
	return o
}
func collectionDrain(t *testing.T, e *Engine, scope core.Scope, id string) Operation {
	t.Helper()
	for range 20 {
		o := collectionWorkerRead(t, e, scope, id)
		if o.State != "queued" {
			return o
		}
		collectionAdvance(t, e)
	}
	t.Fatal("worker did not reach bounded terminal state")
	return Operation{}
}
func collectionPageBodies(t *testing.T, s *core.Store, id string) []string {
	t.Helper()
	rows, err := s.DB().Query(`SELECT body_json FROM history_collection_pages WHERE collection_id=? ORDER BY sequence`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		out = append(out, body)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
func collectionCounts(t *testing.T, s *core.Store, id string) (pages, seen, events int) {
	t.Helper()
	for _, v := range []struct {
		query string
		dest  *int
	}{{`SELECT count(*) FROM history_collection_pages WHERE collection_id=?`, &pages}, {`SELECT count(*) FROM history_collection_seen WHERE collection_id=?`, &seen}, {`SELECT count(*) FROM events WHERE resource_id=?`, &events}} {
		if err := s.DB().QueryRow(v.query, id).Scan(v.dest); err != nil {
			t.Fatal(err)
		}
	}
	return
}
func collectionJoin(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("owned worker did not join")
	}
}

func TestCollectionWorkerCommitsSelectedClosedMergedPages(t *testing.T) {
	f, e, scope, r := collectionWorkerSetup(t, Selection{[]int64{47, 12}})
	var captured []Capture
	for range 11 {
		old := collectionWorkerRead(t, e, scope, r.ID)
		priorCalls := len(f.provider.calls)
		collectionAdvance(t, e)
		current := collectionWorkerRead(t, e, scope, r.ID)
		if current.PageCount != old.PageCount+1 || len(f.provider.calls) != priorCalls+2 {
			t.Fatal("one advance did not commit exactly one resource page")
		}
		bodies := collectionPageBodies(t, f.store, r.ID)
		var c Capture
		if json.Unmarshal([]byte(bodies[len(bodies)-1]), &c) != nil || c.Resource != old.Position.Resource || c.Page != old.Position.Page || c.PullNumber != old.Selection.PullNumbers[old.Position.PullIndex] {
			t.Fatal("durable checkpoint lost exact source selection")
		}
		captured = append(captured, c)
	}
	o := collectionWorkerRead(t, e, scope, r.ID)
	pages, seen, events := collectionCounts(t, f.store, r.ID)
	if o.State != "completed" || pages != 11 || o.PageCount != 11 || o.RecordCount != 12 || seen != 12 || events != 23 {
		t.Fatalf("terminal facts: %+v pages=%d seen=%d events=%d", o, pages, seen, events)
	}
	for _, c := range captured {
		if c.Resource == "pull" {
			var pull reviewprovider.Pull
			if json.Unmarshal(c.Facts, &pull) != nil || pull.State != "closed" || pull.Merged != (pull.Number == 47) {
				t.Fatal("closed/merged history was filtered")
			}
		}
	}
	before := collectionPageBodies(t, f.store, r.ID)
	worked, err := e.advance(t.Context())
	if worked || err != nil || !reflect.DeepEqual(before, collectionPageBodies(t, f.store, r.ID)) || len(f.provider.calls) != 22 {
		t.Fatal("terminal work was retried")
	}
}

func TestCollectionWorkerProviderWaitLeavesCoordinationWriterFree(t *testing.T) {
	f, e, scope, r := collectionWorkerSetup(t, Selection{[]int64{47}})
	entered, release := make(chan struct{}), make(chan struct{})
	f.provider.after = func(c stepCall) error {
		if c.resource == "pull" {
			close(entered)
			<-release
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { _, err := e.advance(t.Context()); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := f.store.Mutate(ctx, scope, core.Mutation{Operation: "worker-proof", Target: scope.WorkerID, Key: "coordination-while-provider", Request: struct{}{}}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		_, err := tx.ExecContext(ctx, `UPDATE workers SET name='Available during provider read' WHERE project_id=? AND id=?`, scope.ProjectID, scope.WorkerID)
		return struct {
			Saved bool `json:"saved"`
		}{true}, err
	})
	close(release)
	collectionJoin(t, done)
	if err != nil {
		t.Fatalf("provider held coordination writer: %v", err)
	}
	o := collectionWorkerRead(t, e, scope, r.ID)
	if o.PageCount != 1 {
		t.Fatal("provider response did not commit")
	}
}

func TestCollectionWorkerCaptureCheckpointSeenAndEventRollbackTogether(t *testing.T) {
	f, e, scope, r := collectionWorkerSetup(t, Selection{[]int64{47}})
	// Claim's revision-two event is allowed. Reject only the capture transition,
	// after body, seen IDs and checkpoint writes have executed in the same Tx.
	if _, err := f.store.DB().Exec(`CREATE TRIGGER collection_worker_fault BEFORE INSERT ON events WHEN NEW.resource_id='` + r.ID + `' AND NEW.revision=3 BEGIN SELECT RAISE(ABORT,'controlled capture event failure'); END`); err != nil {
		t.Fatal(err)
	}
	worked, err := e.advance(t.Context())
	if !worked || err == nil {
		t.Fatal("injected commit failure was ignored")
	}
	o := collectionWorkerRead(t, e, scope, r.ID)
	pages, seen, events := collectionCounts(t, f.store, r.ID)
	if o.State != "running" || o.Revision != 2 || o.Position != Start() || o.PageCount != 0 || o.RecordCount != 0 || pages != 0 || seen != 0 || events != 2 || len(f.provider.calls) != 2 {
		t.Fatalf("partial capture survived rollback: %+v %d/%d/%d", o, pages, seen, events)
	}
}

func TestCollectionWorkerDuplicateAcrossPagesPausesAndResumeRestartsResource(t *testing.T) {
	f, e, scope, r := collectionWorkerSetup(t, Selection{[]int64{47}})
	p := &collectionWorkerProvider{stepProvider: f.provider}
	p.reviewsPage = func(ctx context.Context, repo string, n, page int64) (reviewprovider.Page[reviewprovider.HostedReview], error) {
		err := p.called(stepCall{"reviews", repo, 0, n, page})
		next := int64(0)
		if page == 1 {
			next = 2
		}
		return reviewprovider.Page[reviewprovider.HostedReview]{Items: p.reviews, NextPage: next}, err
	}
	e.provider = p
	collectionAdvance(t, e)
	collectionAdvance(t, e)
	preserved := collectionPageBodies(t, f.store, r.ID)
	collectionAdvance(t, e)
	paused := collectionWorkerRead(t, e, scope, r.ID)
	if paused.State != "paused" || paused.Reason != "source_changed" || paused.Position.Page != 2 || paused.PageCount != 2 || !reflect.DeepEqual(preserved, collectionPageBodies(t, f.store, r.ID)) {
		t.Fatalf("duplicate page changed retained progress: %+v", paused)
	}
	calls := len(p.calls)
	worked, err := e.advance(t.Context())
	if worked || err != nil || len(p.calls) != calls {
		t.Fatal("paused duplicate was retried automatically")
	}
	oldPass := paused.PassNumber
	if _, err := e.Change(t.Context(), scope, r.ID, "resume-shifted", "resume", paused.Revision); err != nil {
		t.Fatal(err)
	}
	resumed := collectionWorkerRead(t, e, scope, r.ID)
	if resumed.Position != (Position{0, "reviews", 1}) || resumed.PassNumber != oldPass+1 {
		t.Fatal("resume reused unstable old page")
	}
	shifted := p.reviews[0]
	shifted.ID = 901
	shifted.Body = "shifted source on explicit new pass"
	p.reviewsPage = func(ctx context.Context, repo string, n, page int64) (reviewprovider.Page[reviewprovider.HostedReview], error) {
		return reviewprovider.Page[reviewprovider.HostedReview]{Items: []reviewprovider.HostedReview{shifted}}, p.called(stepCall{"reviews", repo, 0, n, page})
	}
	collectionAdvance(t, e)
	after := collectionPageBodies(t, f.store, r.ID)
	if len(after) != len(preserved)+1 || !reflect.DeepEqual(after[:len(preserved)], preserved) || !strings.Contains(after[len(after)-1], "shifted source") {
		t.Fatal("resume replaced original captures")
	}
	if o := collectionDrain(t, e, scope, r.ID); o.State != "completed" {
		t.Fatal("explicit resumed pass did not complete", o)
	}
}

func TestCollectionWorkerProviderFailurePausesWithoutRetry(t *testing.T) {
	for _, kind := range []string{"rate", "unavailable", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			f, e, scope, r := collectionWorkerSetup(t, Selection{[]int64{47}})
			collectionAdvance(t, e)
			original := collectionPageBodies(t, f.store, r.ID)
			code, want, status := "provider_unavailable", "source_unavailable", 503
			if kind == "rate" {
				code, want, status = "provider_rate_limited", "rate_limited", 429
			}
			if kind == "invalid" {
				code, want, status = "provider_invalid_response", "source_invalid", 502
			}
			f.provider.after = func(c stepCall) error {
				if c.resource == "reviews" {
					return &reviewprovider.Error{Code: code, Status: status, RetryAfterSeconds: 12, Message: "controlled provider failure"}
				}
				return nil
			}
			collectionAdvance(t, e)
			o := collectionWorkerRead(t, e, scope, r.ID)
			if o.State != "paused" || o.Reason != want || o.PageCount != 1 || !reflect.DeepEqual(original, collectionPageBodies(t, f.store, r.ID)) {
				t.Fatalf("provider failure advanced: %+v", o)
			}
			calls := len(f.provider.calls)
			for range 2 {
				worked, err := e.advance(t.Context())
				if worked || err != nil {
					t.Fatal("paused operation remained scheduled")
				}
			}
			if len(f.provider.calls) != calls {
				t.Fatal("provider failure retried")
			}
		})
	}
}

func TestCollectionWorkerCancellationDiscardsUncommittedResponse(t *testing.T) {
	for _, kind := range []string{"parent-context", "explicit-action"} {
		t.Run(kind, func(t *testing.T) {
			f, e, scope, r := collectionWorkerSetup(t, Selection{[]int64{47}})
			collectionAdvance(t, e)
			original := collectionPageBodies(t, f.store, r.ID)
			entered, release := make(chan struct{}), make(chan struct{})
			f.provider.after = func(c stepCall) error {
				if c.resource == "reviews" {
					close(entered)
					<-release
				}
				return nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := e.advance(ctx); done <- err }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("provider did not block")
			}
			if kind == "parent-context" {
				cancel()
			} else {
				o := collectionWorkerRead(t, e, scope, r.ID)
				if _, err := e.Change(t.Context(), scope, r.ID, "cancel-inflight", "cancel", o.Revision); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			collectionJoin(t, done)
			o := collectionWorkerRead(t, e, scope, r.ID)
			want := "running"
			if kind == "explicit-action" {
				want = "canceled"
			}
			if o.State != want || o.PageCount != 1 || o.Position != (Position{0, "reviews", 1}) || !reflect.DeepEqual(original, collectionPageBodies(t, f.store, r.ID)) {
				t.Fatal("cancel acknowledged or retained response", o)
			}
		})
	}
}

func TestCollectionWorkerBindingRecheckedBeforeReadAndCommit(t *testing.T) {
	for _, boundary := range []string{"before-read", "before-commit"} {
		for _, kind := range []string{"revoked", "fingerprint", "repository"} {
			t.Run(boundary+"/"+kind, func(t *testing.T) {
				f, e, scope, r := collectionWorkerSetup(t, Selection{[]int64{47}})
				mutate := func() {
					t.Helper()
					var err error
					switch kind {
					case "revoked":
						_, err = f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE principal_id=?`, scope.PrincipalID)
					case "repository":
						_, err = f.store.DB().Exec(`UPDATE projects SET github_repository_id=124 WHERE id=?`, scope.ProjectID)
					case "fingerprint":
						e.fingerprint = strings.Repeat("b", 64)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if boundary == "before-read" {
					mutate()
				} else {
					f.provider.after = func(c stepCall) error {
						if c.resource == "pull" {
							mutate()
						}
						return nil
					}
				}
				collectionAdvance(t, e)
				var state, reason string
				var count int
				if err := f.store.DB().QueryRow(`SELECT state,reason,page_count FROM history_collections WHERE id=?`, r.ID).Scan(&state, &reason, &count); err != nil {
					t.Fatal(err)
				}
				expected := "configuration_changed"
				if kind == "revoked" {
					expected = "authorization_changed"
				}
				pages, seen, _ := collectionCounts(t, f.store, r.ID)
				if state != "paused" || reason != expected || count != 0 || pages != 0 || seen != 0 {
					t.Fatalf("changed binding retained provider result: %s/%s pages=%d", state, reason, pages)
				}
				calls := 0
				if boundary == "before-commit" {
					calls = 2
				}
				if len(f.provider.calls) != calls {
					t.Fatal("binding refusal performed unexpected provider work")
				}
			})
		}
	}
}

func TestCollectionWorkerRunCancelJoinRestartNewPass(t *testing.T) {
	f, e, scope, r := collectionWorkerSetup(t, Selection{[]int64{47}})
	collectionAdvance(t, e)
	collectionAdvance(t, e)
	before := collectionWorkerRead(t, e, scope, r.ID)
	original := collectionPageBodies(t, f.store, r.ID)
	if before.Position != (Position{0, "reviews", 2}) {
		t.Fatal("fixture lacks unfinished resource")
	}
	entered := make(chan struct{})
	p := &collectionWorkerProvider{stepProvider: f.provider}
	p.reviewsPage = func(ctx context.Context, repo string, n, page int64) (reviewprovider.Page[reviewprovider.HostedReview], error) {
		if err := p.called(stepCall{"reviews", repo, 0, n, page}); err != nil {
			return reviewprovider.Page[reviewprovider.HostedReview]{}, err
		}
		close(entered)
		<-ctx.Done()
		return reviewprovider.Page[reviewprovider.HostedReview]{}, ctx.Err()
	}
	e.provider = p
	ctx, cancel := context.WithCancel(t.Context())
	done, err := e.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("worker did not claim unfinished page")
	}
	cancel()
	collectionJoin(t, done)
	interrupted := collectionWorkerRead(t, e, scope, r.ID)
	if interrupted.State != "running" || interrupted.Position.Page != 1 || interrupted.PassNumber != before.PassNumber+1 || interrupted.PageCount != 2 || !reflect.DeepEqual(original, collectionPageBodies(t, f.store, r.ID)) {
		t.Fatal("canceled run retained response", interrupted)
	}
	restarted, err := New(f.store, f.provider, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	done2, err := restarted.Run(ctx2)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		o := collectionWorkerRead(t, restarted, scope, r.ID)
		if o.State == "completed" {
			if o.PassNumber <= interrupted.PassNumber || o.PageCount <= 2 {
				t.Fatal("restart did not start new pass")
			}
			break
		}
		select {
		case <-deadline:
			cancel2()
			collectionJoin(t, done2)
			t.Fatal("restart did not complete")
		case <-time.After(time.Millisecond):
		}
	}
	cancel2()
	collectionJoin(t, done2)
	after := collectionPageBodies(t, f.store, r.ID)
	if !reflect.DeepEqual(after[:len(original)], original) {
		t.Fatal("restart rewrote earlier committed pages")
	}
	var c Capture
	if json.Unmarshal([]byte(after[2]), &c) != nil || c.Resource != "reviews" || c.Page != 1 {
		t.Fatal("restart reused old provider offset")
	}
}

func TestCollectionWorkerWriterContentionRetainsCapturedPageWithoutRefetch(t *testing.T) {
	f, e, scope, r := collectionWorkerSetup(t, Selection{[]int64{47}})
	// A separate SQLite writer acquires the actual WAL write lock only after
	// the worker has claimed its page and entered provider work.
	entered, release := make(chan struct{}), make(chan struct{})
	f.provider.after = func(c stepCall) error {
		if c.resource == "pull" {
			close(entered)
			<-release
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { _, err := e.advance(t.Context()); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	lock, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	if _, err = lock.Exec(`UPDATE workers SET name=name WHERE project_id=? AND id=?`, scope.ProjectID, scope.WorkerID); err != nil {
		lock.Rollback()
		close(release)
		t.Fatal(err)
	}
	close(release)
	// The independent lock is real, not a mocked overload. The response must
	// stay in memory until the bounded local retry can acquire the writer.
	select {
	case err := <-done:
		lock.Rollback()
		t.Fatalf("worker abandoned captured response under writer contention: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	collectionJoin(t, done)
	o := collectionWorkerRead(t, e, scope, r.ID)
	if o.PageCount != 1 || len(f.provider.calls) != 2 {
		t.Fatal("local contention repeated provider acquisition", o, len(f.provider.calls))
	}
	pages, seen, events := collectionCounts(t, f.store, r.ID)
	if pages != 1 || seen != 1 || events != 3 {
		t.Fatalf("contended page partially/duplicately committed: %d/%d/%d", pages, seen, events)
	}
}

func TestCollectionWorkerQueuedContinuationRestartStartsNewPass(t *testing.T) {
	f, e, scope, r := collectionWorkerSetup(t, Selection{[]int64{47}})
	collectionAdvance(t, e)
	collectionAdvance(t, e)
	before := collectionWorkerRead(t, e, scope, r.ID)
	preserved := collectionPageBodies(t, f.store, r.ID)
	if before.State != "queued" || before.Position != (Position{0, "reviews", 2}) || before.PageCount != 2 {
		t.Fatal("fixture did not stop after committed page one")
	}
	// Nothing is in flight: this is a process boundary after capture commit but
	// before the next claim, unlike the existing running-page cancellation test.
	restarted, err := New(f.store, f.provider, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var first stepCall
	f.provider.after = func(call stepCall) error {
		if call.resource == "reviews" && first.resource == "" {
			first = call
			close(entered)
			<-release
		}
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := restarted.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("restarted provider did not enter")
	}
	current := collectionWorkerRead(t, restarted, scope, r.ID)
	if first.page != 1 || current.Position != (Position{0, "reviews", 1}) || current.PassNumber != before.PassNumber+1 || current.Revision <= before.Revision || current.PageCount != before.PageCount {
		close(release)
		cancel()
		collectionJoin(t, done)
		t.Fatal("queued provider continuation was reused after restart")
	}
	close(release)
	deadline := time.After(2 * time.Second)
	for {
		o := collectionWorkerRead(t, restarted, scope, r.ID)
		if o.State == "completed" {
			break
		}
		select {
		case <-deadline:
			cancel()
			collectionJoin(t, done)
			t.Fatal("restarted queued collection did not finish")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	collectionJoin(t, done)
	after := collectionPageBodies(t, f.store, r.ID)
	if len(after) <= len(preserved) || !reflect.DeepEqual(after[:len(preserved)], preserved) {
		t.Fatal("queued restart changed committed capture bytes")
	}
	var firstNew Capture
	if json.Unmarshal([]byte(after[len(preserved)]), &firstNew) != nil || firstNew.Resource != "reviews" || firstNew.Page != 1 {
		t.Fatal("retained restarted capture is not page one")
	}
	tx, err := f.store.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := ValidateSnapshot(t.Context(), tx); err != nil {
		t.Fatal("genuine queued restart did not conserve a valid retained snapshot", err)
	}
}
