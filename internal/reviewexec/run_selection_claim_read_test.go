package reviewexec

import (
	"context"
	"testing"
	"time"
)

func TestRunSelectionClaimedExplicitReadContention(t *testing.T) {
	for _, tc := range []struct {
		name, state, reason string
		calls, children     int
	}{
		{name: "brief", state: "selected", calls: 3, children: 1},
		{name: "exhausted", state: "held", reason: "local_read_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := selectionFixture(t)
			receipt, _ := selectionEnqueue(t, f, "post-claim-reader", "cli", nil)
			r := &refreshTestReader{}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()

			// Pause at the existing post-commit notification, not at the
			// provider callback (which would exercise the later prefix read).
			f.e.operationMu.Lock()
			unlocked := false
			defer func() {
				if !unlocked {
					f.e.operationMu.Unlock()
				}
			}()
			type result struct {
				worked bool
				err    error
			}
			done := make(chan result, 1)
			go func() {
				worked, err := f.e.runNext(ctx, map[string]TargetReader{"test-capability": r})
				done <- result{worked, err}
			}()
			claimDeadline := time.Now().Add(4 * time.Second)
			for {
				var state string
				var revision int64
				err := f.store.DB().QueryRowContext(ctx, "SELECT state,revision FROM review_selections WHERE id=?", receipt.ID).Scan(&state, &revision)
				if err != nil {
					t.Fatal(err)
				}
				if state == "selecting" && revision == 2 {
					break
				}
				if time.Now().After(claimDeadline) {
					t.Fatal("selection claim did not commit", state, revision)
				}
				time.Sleep(5 * time.Millisecond)
			}
			db := f.store.DB()
			db.SetMaxOpenConns(1)
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			waitCount := db.Stats().WaitCount
			f.e.operationMu.Unlock()
			unlocked = true

			if tc.name == "brief" {
				// A second wait proves the first bounded admission expired.
				deadline := time.NewTimer(5 * time.Second)
				defer deadline.Stop()
				tick := time.NewTicker(5 * time.Millisecond)
				defer tick.Stop()
				for db.Stats().WaitCount < waitCount+2 {
					select {
					case result := <-done:
						t.Fatalf("claimed selection stopped instead of retrying: %+v", result)
					case <-deadline.C:
						t.Fatal("no second read admission after expiry")
					case <-tick.C:
					}
				}
				if r.calls.Load() != 0 {
					t.Fatal("provider called before local admission")
				}
				conn.Close()
			}
			select {
			case result := <-done:
				if result.err != nil || !result.worked {
					t.Fatalf("claimed selection did not finish locally: %+v", result)
				}
			case <-ctx.Done():
				t.Fatal("claimed selection did not finish", ctx.Err())
			}
			conn.Close()
			got := selectionContentionRead(t, f, receipt.ID)
			if got.State != tc.state || got.Reason != tc.reason || got.Revision != 3 ||
				int(r.calls.Load()) != tc.calls || attemptCount(t, f.store, "SELECT count(*) FROM review_operations") != tc.children {
				t.Fatal("unexpected state or duplicate work", got, r.calls.Load())
			}
		})
	}
}
