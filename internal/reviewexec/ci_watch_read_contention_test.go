package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func holdCIWatchReadPool(t *testing.T, f *attemptFixture) func() {
	t.Helper()
	f.store.DB().SetMaxOpenConns(1)
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(release)
	return release
}

func TestCIWatchInitialReadContentionUsesOriginalWaitDeadline(t *testing.T) {
	f, s, _, r, scope := ciWatchFixture(t)
	before := lifecycleHintCounts(t, f)
	release := holdCIWatchReadPool(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := make(chan ciWatchReply, 1)
	started := time.Now()
	target := CIWatchTarget{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}
	go func() { v, err := f.e.WatchCI(ctx, scope, []CIWatchTarget{target}, 3); out <- ciWatchReply{v, err} }()
	select {
	case reply := <-out:
		release()
		t.Fatal("quiet watch ended on transient initial read admission", reply.err)
	case <-time.After(2250 * time.Millisecond):
	}
	ciWatchRegistered(t, s, 0)
	release()
	reply := receiveCIWatch(t, out)
	if reply.err != nil || reply.value.State != "pending" || !reply.value.TimedOut || reply.value.Targets[0].CIWatchTarget != target || reply.value.Targets[0].Reason != "ci_watch_check_pending" {
		t.Fatal("quiet timeout lost real pending state", reply)
	}
	if elapsed := time.Since(started); elapsed < 2800*time.Millisecond || elapsed > 4500*time.Millisecond {
		t.Fatal("initial admission restarted selected wait budget", elapsed)
	}
	ciWatchRegistered(t, s, 0)
	r.mu.Lock()
	calls := r.calls
	r.mu.Unlock()
	if calls != 0 || !reflect.DeepEqual(before, lifecycleHintCounts(t, f)) {
		t.Fatal("read retry acquired provider work or changed authority", calls)
	}
}

func notifyCIWatchRead(s *lifecycleSchedule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.watchers {
		select {
		case sub.changed <- struct{}{}:
		default:
		}
	}
}

func TestCIWatchNotifiedReadContentionReauthorizesAndCleansUp(t *testing.T) {
	f, s, _, r, scope := ciWatchFixture(t)
	target := CIWatchTarget{PullNumber: 43, HeadSHA: strings.Repeat("a", 40)}
	before := lifecycleHintCounts(t, f)
	for _, phase := range []string{"fresh_result", "caller_cancel", "lifetime_cancel", "zero_wait", "revoked"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var cancelLifetime context.CancelFunc
			if phase == "lifetime_cancel" {
				f.e.resultWaitLifetime, cancelLifetime = context.WithCancel(t.Context())
				defer func() { cancelLifetime(); f.e.resultWaitLifetime = nil }()
			}
			out := make(chan ciWatchReply, 1)
			var release func()
			wait := 300
			if phase == "zero_wait" {
				wait = 0
				release = holdCIWatchReadPool(t, f)
			}
			go func() { v, err := f.e.WatchCI(ctx, scope, []CIWatchTarget{target}, wait); out <- ciWatchReply{v, err} }()
			if phase != "zero_wait" {
				ciWatchRegistered(t, s, 1)
				release = holdCIWatchReadPool(t, f)
				notifyCIWatchRead(s)
			}
			if phase == "caller_cancel" || phase == "lifetime_cancel" {
				until := time.Now().Add(time.Second)
				initial := f.store.DB().Stats().WaitCount
				for f.store.DB().Stats().WaitCount == initial && time.Now().Before(until) {
					time.Sleep(time.Millisecond)
				}
				if phase == "caller_cancel" {
					cancel()
				} else {
					cancelLifetime()
				}
			} else if phase != "zero_wait" {
				select {
				case reply := <-out:
					release()
					t.Fatal("notification read admission ended quiet watch", reply.err)
				case <-time.After(2250 * time.Millisecond):
				}
			}
			if phase == "revoked" {
				if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `UPDATE credentials SET revoked=1 WHERE principal_id=?`, scope.PrincipalID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if phase != "zero_wait" {
				release()
			}
			if phase == "fresh_result" {
				r.mu.Lock()
				r.states[43] = "satisfied"
				r.mu.Unlock()
				watchTick(t, f, s)
			}
			var reply ciWatchReply
			select {
			case reply = <-out:
			case <-time.After(3 * time.Second):
				release()
				t.Fatal("read retry did not terminate")
			}
			release()
			if phase == "fresh_result" {
				if reply.err != nil || reply.value.State != "passed" || reply.value.Targets[0].CIWatchTarget != target {
					t.Fatal("fresh coherent result lost", reply)
				}
			} else {
				var failure *core.Error
				want := "overloaded"
				if phase == "revoked" {
					want = "unauthorized"
				}
				if !errors.As(reply.err, &failure) || failure.Code != want || reply.value.ProjectID != "" {
					t.Fatal("read failure returned stale facts or wrong error", phase, reply)
				}
				if phase == "lifetime_cancel" {
					details, ok := failure.Details.(map[string]string)
					if !ok || details["reason"] != "result_wait_lifetime" {
						t.Fatal("lost lifetime cancellation cause", failure)
					}
				}
			}
			ciWatchRegistered(t, s, 0)
			s.mu.Lock()
			entries, principals := len(s.watches), len(s.watchPrincipals)
			s.mu.Unlock()
			if entries != 0 || principals != 0 {
				t.Fatal("temporary subscriber capacity leaked", entries, principals)
			}
		})
	}
	r.mu.Lock()
	calls := r.calls
	r.mu.Unlock()
	if calls != 1 || !reflect.DeepEqual(before, lifecycleHintCounts(t, f)) {
		t.Fatal("read admission retries duplicated provider work or changed review authority", calls)
	}
}
