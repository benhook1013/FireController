package reviewexec

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func checkpointOverloadLoop(t *testing.T, f *attemptFixture, p *checkpointFake) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- f.e.checkpointLoop(ctx, map[string]CheckpointProvider{"test-capability": p})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("checkpoint contention worker did not join")
		}
	})
	return cancel, done
}

func checkpointOverloadAlive(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatal("local overload stopped checkpoint worker", err)
	case <-time.After(2200 * time.Millisecond):
	}
}

func checkpointOverloadPublished(t *testing.T, f *attemptFixture, p *checkpointFake, id string) {
	t.Helper()
	v := checkpointTerminal(t, f, id)
	if v.State != "published" || v.Revision != 4 || v.CommentID != 800 {
		t.Fatal("original publication did not settle exactly once", v)
	}
	posts, reads := p.counts()
	if posts != 1 || reads != 0 {
		t.Fatal("local retry repeated provider work", posts, reads)
	}
	for _, q := range []struct {
		sql  string
		want int
	}{
		{`SELECT count(*) FROM review_checkpoint_revisions WHERE publication_id=?`, 4},
		{`SELECT count(*) FROM review_checkpoint_intents WHERE publication_id=?`, 1},
		{`SELECT count(*) FROM events WHERE resource_id=? AND event_type='reviews.checkpoint_publication'`, 4},
	} {
		var n int
		if err := f.store.DB().QueryRowContext(t.Context(), q.sql, id).Scan(&n); err != nil || n != q.want {
			t.Fatal("retry changed journal/event multiplicity", n, q.want, err)
		}
	}
}

func TestCheckpointOverloadSelectionAndClaim(t *testing.T) {
	for _, phase := range []string{"selection", "claim", "startup"} {
		t.Run(phase, func(t *testing.T) {
			f, _, evidence := checkpointFixture(t, 0)
			r, _ := checkpointRequest(t, f, evidence, "overload-"+phase)
			p := &checkpointFake{}
			var unlock func()
			if phase == "selection" {
				f.store.DB().SetMaxOpenConns(1)
				conn, err := f.store.DB().Conn(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { conn.Close() })
				unlock = func() {
					if err := conn.Close(); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				tx := refreshContentionLock(t, f)
				unlock = func() {
					if err := tx.Rollback(); err != nil {
						t.Fatal(err)
					}
				}
			}
			if phase == "startup" {
				type started struct {
					done <-chan error
					err  error
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				ready := make(chan started, 1)
				go func() {
					done, err := f.e.StartCheckpointPublications(ctx, map[string]CheckpointProvider{"test-capability": p})
					ready <- started{done, err}
				}()
				select {
				case result := <-ready:
					t.Fatal("startup overload was fatal", result.err)
				case <-time.After(2200 * time.Millisecond):
				}
				unlock()
				select {
				case result := <-ready:
					if result.err != nil {
						t.Fatal(result.err)
					}
					defer checkpointStop(t, cancel, result.done)
				case <-time.After(3 * time.Second):
					t.Fatal("startup did not recover")
				}
			} else {
				_, done := checkpointOverloadLoop(t, f, p)
				checkpointOverloadAlive(t, done)
				if posts, _ := p.counts(); posts != 0 {
					t.Fatal("posted before local claim")
				}
				unlock()
			}
			checkpointOverloadPublished(t, f, p, r.ID)
		})
	}
}

func checkpointOverloadPost(t *testing.T, f *attemptFixture, evidence string) (Receipt, *checkpointFake, context.CancelFunc, <-chan error, *sql.Tx) {
	t.Helper()
	r, _ := checkpointRequest(t, f, evidence, "overload-finish")
	p := &checkpointFake{entered: make(chan struct{}), release: make(chan struct{})}
	cancel, done := checkpointOverloadLoop(t, f, p)
	select {
	case <-p.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("provider post not reached")
	}
	tx := refreshContentionLock(t, f)
	close(p.release)
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		committed := len(p.comments) == 1
		p.mu.Unlock()
		if committed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture provider did not retain successful comment")
		}
		time.Sleep(time.Millisecond)
	}
	return r, p, cancel, done, tx
}

func TestCheckpointOverloadSuccessfulPostRetainsResult(t *testing.T) {
	f, _, evidence := checkpointFixture(t, 0)
	_, sourceDone := refreshContentionLoop(t, f, &refreshTestReader{})
	r, p, _, done, tx := checkpointOverloadPost(t, f, evidence)
	checkpointOverloadAlive(t, done)
	select {
	case err := <-sourceDone:
		t.Fatal("unrelated source worker stopped", err)
	default:
	}
	if v := checkpointRead(t, f, r.ID); v.State != "posting" || v.Revision != 3 {
		t.Fatal("lost original posting intent", v)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	checkpointOverloadPublished(t, f, p, r.ID)
}

func TestCheckpointOverloadPrePostGuardReauthorizes(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "revoked"}[revoke], func(t *testing.T) {
			f, _, evidence := checkpointFixture(t, 0)
			r, _ := checkpointRequest(t, f, evidence, "overload-intent")
			entered, release := make(chan struct{}), make(chan struct{})
			p := &checkpointFake{beforeActor: func() { close(entered); <-release }}
			_, done := checkpointOverloadLoop(t, f, p)
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("pre-post actor lookup not reached")
			}
			tx := refreshContentionLock(t, f)
			if revoke {
				if _, err := tx.ExecContext(t.Context(), `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			checkpointOverloadAlive(t, done)
			if posts, _ := p.counts(); posts != 0 {
				t.Fatal("uncommitted intent authorized post")
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if !revoke {
				checkpointOverloadPublished(t, f, p, r.ID)
			} else {
				v := checkpointTerminal(t, f, r.ID)
				if v.State != "held" || v.Reason != "requester_revoked" || v.Revision != 3 {
					t.Fatal("retry ignored changed authorization", v)
				}
				if posts, _ := p.counts(); posts != 0 {
					t.Fatal("revoked requester posted")
				}
				var n int
				if err := f.store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM review_checkpoint_intents WHERE publication_id=?`, r.ID).Scan(&n); err != nil || n != 0 {
					t.Fatal("unposted hold retained an intent", n, err)
				}
			}
			p.mu.Lock()
			repositories, actors := p.repositories, p.actors
			p.mu.Unlock()
			if repositories != 1 || actors != 1 {
				t.Fatal("retried provider preparation", repositories, actors)
			}
		})
	}
}

func TestCheckpointOverloadCancellationRecoversWithoutRepost(t *testing.T) {
	f, _, evidence := checkpointFixture(t, 0)
	r, p, cancel, done, tx := checkpointOverloadPost(t, f, evidence)
	checkpointOverloadAlive(t, done)
	start := time.Now()
	checkpointStop(t, cancel, done)
	if time.Since(start) > time.Second {
		t.Fatal("local retry ignored cancellation")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if v := checkpointRead(t, f, r.ID); v.State != "posting" || v.Revision != 3 {
		t.Fatal("cancellation lost intent", v)
	}
	p.mu.Lock()
	p.entered = nil
	p.release = nil
	p.mu.Unlock()
	cancel, done = checkpointStart(t, f.e, p)
	defer checkpointStop(t, cancel, done)
	if v := checkpointTerminal(t, f, r.ID); v.State != "published" {
		t.Fatal(v)
	}
	if posts, reads := p.counts(); posts != 1 || reads != 1 {
		t.Fatal("restart reposted", posts, reads)
	}
}

// Spend the real provider budget, leaving a small original-budget remainder for
// local admission. This does not change the worker's fifteen-second deadline.
type checkpointBudgetProvider struct {
	*checkpointFake
	first    atomic.Bool
	deadline chan time.Time
}

func (p *checkpointBudgetProvider) CurrentActor(ctx context.Context) (reviewprovider.Author, error) {
	if p.first.CompareAndSwap(false, true) {
		deadline, _ := ctx.Deadline()
		p.deadline <- deadline
		timer := time.NewTimer(time.Until(deadline) - 250*time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return reviewprovider.Author{}, ctx.Err()
		}
	}
	return p.checkpointFake.CurrentActor(ctx)
}

func TestCheckpointIntentBudgetRefusedAdmissionHoldsAndExplicitlyRetries(t *testing.T) {
	f, _, evidence := checkpointFixture(t, 0)
	r, _ := checkpointRequest(t, f, evidence, "intent-budget")
	_, sourceDone := refreshContentionLoop(t, f, &refreshTestReader{})
	p := &checkpointBudgetProvider{checkpointFake: &checkpointFake{}, deadline: make(chan time.Time, 1)}
	cancel, done := checkpointStart(t, f.e, p)
	defer checkpointStop(t, cancel, done)
	var deadline time.Time
	select {
	case deadline = <-p.deadline:
	case <-time.After(2 * time.Second):
		t.Fatal("provider preparation not reached")
	}
	tx := refreshContentionLock(t, f)
	// Admission stays refused until after the unchanged action deadline. The
	// terminal hold itself can settle only after this writer is released.
	select {
	case err := <-done:
		t.Fatal("pre-intent admission expiry stopped worker", err)
	case <-time.After(time.Until(deadline) + 100*time.Millisecond):
	}
	select {
	case err := <-sourceDone:
		t.Fatal("unrelated source worker stopped", err)
	default:
	}
	if posts, _ := p.counts(); posts != 0 {
		t.Fatal("expired admission posted")
	}
	if n := attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_intents`); n != 0 {
		t.Fatal("refused admission retained intent", n)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	v := checkpointTerminal(t, f, r.ID)
	if v.State != "held" || v.Reason != "local_storage_overloaded" || v.Revision != 3 {
		t.Fatal("expired admission did not settle as a single unposted hold", v)
	}
	// This public guarded retry succeeds only when the hold retained Unposted.
	checkpointRecover(t, f, v, "intent-budget-explicit-retry", true)
	v = checkpointTerminal(t, f, r.ID)
	if v.State != "published" || v.CommentID != 800 {
		t.Fatal("safe explicit retry did not publish", v)
	}
	if posts, reads := p.counts(); posts != 1 || reads != 0 {
		t.Fatal("retry repeated provider action", posts, reads)
	}
	if n := attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_intents`); n != 1 {
		t.Fatal("retry accumulated intents", n)
	}
}

func TestCheckpointIntentBudgetEnteredTransactionTimeoutRemainsFatal(t *testing.T) {
	f, _, evidence := checkpointFixture(t, 0)
	r, _ := checkpointRequest(t, f, evidence, "intent-entered-timeout")
	// This local statement enters the intent callback and exhausts its remaining
	// action budget. It is not a refused admission and must not become a safe hold.
	admissionSQL(t, f, `CREATE TRIGGER checkpoint_slow_intent BEFORE INSERT ON review_checkpoint_intents BEGIN SELECT sum(x) FROM (WITH RECURSIVE work(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM work WHERE x<1000000000) SELECT x FROM work); END`)
	p := &checkpointBudgetProvider{checkpointFake: &checkpointFake{}, deadline: make(chan time.Time, 1)}
	cancel, done := checkpointStart(t, f.e, p)
	defer cancel()
	select {
	case err := <-done:
		attemptCode(t, err, "checkpoint_publication_persistence_failed")
	case <-time.After(18 * time.Second):
		t.Fatal("entered intent timeout was not reported")
	}
	if v := checkpointRead(t, f, r.ID); v.State != "preparing" || v.Revision != 2 {
		t.Fatal("entered timeout was labeled safely unposted", v)
	}
	if posts, _ := p.counts(); posts != 0 {
		t.Fatal("failed intent posted")
	}
	if n := attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_intents`); n != 0 {
		t.Fatal("timed-out transaction did not roll back", n)
	}
}

func TestCheckpointIntentBudgetCompletedCallbackCommitFailureRemainsFatal(t *testing.T) {
	f, _, evidence := checkpointFixture(t, 0)
	r, _ := checkpointRequest(t, f, evidence, "intent-commit-failure")
	admissionSQL(t, f, `CREATE TABLE checkpoint_commit_guard (publication_id TEXT REFERENCES review_checkpoint_publications(id) DEFERRABLE INITIALLY DEFERRED)`)
	admissionSQL(t, f, `CREATE TRIGGER checkpoint_bad_commit AFTER INSERT ON review_checkpoint_intents BEGIN INSERT INTO checkpoint_commit_guard VALUES('missing-publication'); END`)
	p := &checkpointFake{}
	cancel, done := checkpointStart(t, f.e, p)
	defer cancel()
	select {
	case err := <-done:
		attemptCode(t, err, "checkpoint_publication_persistence_failed")
	case <-time.After(3 * time.Second):
		t.Fatal("completed callback commit failure was retried or hidden")
	}
	if v := checkpointRead(t, f, r.ID); v.State != "preparing" || v.Revision != 2 {
		t.Fatal("commit failure changed retained claim", v)
	}
	if n := attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_intents`); n != 0 {
		t.Fatal("failed commit retained partial intent", n)
	}
	if posts, _ := p.counts(); posts != 0 {
		t.Fatal("failed commit authorized a post")
	}
}
