package reviewexec

import (
	"context"
	"testing"
	"time"
)

// Each retained-result API must use the same fixed wait budget when its first
// authorized read loses the two-second pool admission window.
func TestQuietResultReadsSurviveAdmissionContention(t *testing.T) {
	for _, kind := range []string{"source", "checkpoint", "selection", "compact-operation"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			var f *attemptFixture
			var wait func(context.Context) (string, error)
			switch kind {
			case "source":
				f = sourceHTTPFixture(t)
				receipt := refreshEnqueue(t, f, "read-admission", refreshRequest())
				scope := evidenceReadScope(t, f, attemptProject, f.reader)
				wait = func(ctx context.Context) (string, error) {
					v, e := f.e.WaitSourceRefresh(ctx, scope, receipt.ID, 3*time.Second)
					return v.State, e
				}
			case "checkpoint":
				var evidence string
				f, _, evidence = checkpointFixture(t, 0)
				receipt, _ := checkpointRequest(t, f, evidence, "read-admission")
				scope := evidenceReadScope(t, f, attemptProject, f.reader)
				wait = func(ctx context.Context) (string, error) {
					v, _, e := f.e.WaitCheckpointPublication(ctx, scope, receipt.ID, 3)
					return v.State, e
				}
			case "selection":
				f = selectionFixture(t)
				receipt, _ := selectionEnqueue(t, f, "read-admission", "cli", nil)
				scope := selectionScope(t, f, attemptProject, f.writer, "read")
				wait = func(ctx context.Context) (string, error) {
					v, _, e := f.e.WaitRun(ctx, scope, receipt.ID, 3)
					return v.State, e
				}
			case "compact-operation":
				f = sourceHTTPFixture(t)
				receipt := f.enqueue(t, attemptProject, "read-admission", "cli", 2, 1)
				scope := operationReadScope(t, f)
				wait = func(ctx context.Context) (string, error) {
					v, e := f.e.WaitOperationStatus(ctx, scope, receipt.ID, 3)
					return v.State, e
				}
			}
			events := attemptCount(t, f.store, `SELECT count(*) FROM events`)
			f.store.DB().SetMaxOpenConns(1)
			conn, err := f.store.DB().Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type result struct {
				state string
				err   error
			}
			done := make(chan result, 1)
			go func() { state, err := wait(ctx); done <- result{state, err} }()
			// Beyond the storage admission deadline but inside the selected wait.
			select {
			case got := <-done:
				t.Fatalf("wait ended before pool recovery: %+v", got)
			case <-time.After(2200 * time.Millisecond):
			}
			if err = conn.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				if got.err != nil || got.state != "queued" {
					t.Fatalf("recovery changed retained result: %+v", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("read recovery reset or exceeded the selected wait")
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM events`) != events {
				t.Fatal("read retry wrote events")
			}
		})
	}
}
