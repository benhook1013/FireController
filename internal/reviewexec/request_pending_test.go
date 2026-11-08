package reviewexec

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func requirePendingRequest(t *testing.T, err error, id string) {
	t.Helper()
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "review_inflight" || failure.Status != 409 {
		t.Fatalf("expected pending request refusal, got %v", err)
	}
	details, ok := failure.Details.(map[string]string)
	if !ok || details["operation_id"] != id {
		t.Fatalf("missing original operation: %#v", failure.Details)
	}
}

func TestPendingRequestAutomaticHostedRejectsManualDuplicate(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	run := schedulingLatest(t, f)
	if run.State != "selected" || run.OperationID == "" {
		t.Fatal(run)
	}
	scope := selectionScope(t, f, attemptProject, f.writer, "write")
	op, err := f.e.ReadOperation(t.Context(), scope, run.OperationID)
	if err != nil || op.State != "queued" {
		t.Fatal(op, err)
	}
	before := selectionCounts(t, f)
	_, err = f.e.Enqueue(t.Context(), scope, "manual-after-automatic", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "hosted", HeadSHA: op.HeadSHA})
	requirePendingRequest(t, err, op.ID)
	if selectionCounts(t, f) != before {
		t.Fatal("duplicate changed durable counts")
	}
}

func TestPendingRequestStateReplayAndChannelIsolation(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, state := range []string{"queued", "preparing", "posting", "active", "uncertain", "held", "completed", "cancelled", "failed", "rate_limited"} {
			t.Run(channel+"/"+state, func(t *testing.T) {
				f := newAttemptFixture(t)
				f.configure(t, attemptProject, "test-capability", 0, false)
				original := f.enqueue(t, attemptProject, "original", channel, 2, 1)
				admissionSQL(t, f, `UPDATE review_operations SET state=? WHERE id=?`, state, original.ID)
				scope := f.scope(t, attemptProject, f.writer, false)
				request := Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: channel, HeadSHA: strings.Repeat("a", 40)}
				replay, err := f.e.Enqueue(t.Context(), scope, "original", request)
				var receipt Receipt
				if err != nil || json.Unmarshal(replay, &receipt) != nil || receipt.ID != original.ID {
					t.Fatal("original replay changed", string(replay), err)
				}
				before := attemptCount(t, f.store, `SELECT count(*) FROM review_operations`)
				// A different head is not a bypass for a pending operation.
				request.HeadSHA = strings.Repeat("b", 40)
				_, err = f.e.Enqueue(t.Context(), scope, "new-key", request)
				pending := state == "queued" || state == "preparing" || state == "posting" || state == "active" || state == "uncertain"
				if pending {
					requirePendingRequest(t, err, original.ID)
					if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != before {
						t.Fatal("duplicate created operation")
					}
				} else if err != nil {
					t.Fatal("terminal operation blocked request", err)
				}
				other := "hosted"
				if channel == "hosted" {
					other = "cli"
				}
				f.enqueue(t, attemptProject, "independent-channel", other, 2, 1)
			})
		}
	}
}

func TestPendingRequestConcurrentAdmission(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		t.Run(channel, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			scope := f.scope(t, attemptProject, f.writer, false)
			request := Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: channel, HeadSHA: strings.Repeat("a", 40)}
			type result struct {
				data json.RawMessage
				err  error
			}
			results := make(chan result, 2)
			start := make(chan struct{})
			for _, key := range []string{"first", "second"} {
				go func() {
					<-start
					data, err := f.e.Enqueue(t.Context(), scope, key, request)
					results <- result{data, err}
				}()
			}
			close(start)
			a, b := <-results, <-results
			if a.err != nil {
				a, b = b, a
			}
			var receipt Receipt
			if a.err != nil || json.Unmarshal(a.data, &receipt) != nil {
				t.Fatal("neither request admitted", a.err, b.err)
			}
			requirePendingRequest(t, b.err, receipt.ID)
			if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 {
				t.Fatal("concurrent duplicate admitted")
			}
		})
	}
}
