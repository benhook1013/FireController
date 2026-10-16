package reviewexec

import (
	"context"
	"database/sql"
	"testing"
	"time"
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
