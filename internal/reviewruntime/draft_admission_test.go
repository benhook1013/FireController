package reviewruntime

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

func setRuntimeDraft(f *cliDriverFixture, draft bool) {
	pull := f.reader.pulls[2]
	pull.IsDraft = &draft
	f.reader.pulls[2] = pull
}

func waitRuntimeDraftState(t *testing.T, f *cliDriverFixture, driver reviewexec.Driver, id, channel string) reviewexec.Operation {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done, err := f.engine.Start(ctx, map[string]reviewexec.Driver{f.capability.ID: driver})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	waitSeconds := 3
	if channel == "cli" {
		waitSeconds = 1
	}
	operation, terminal, readErr := f.engine.WaitOperation(t.Context(), f.scope(t, f.writer, false), id, waitSeconds)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("draft hold did not stop cleanly")
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	if channel == "cli" {
		if terminal || operation.State != "queued" || operation.Reason != "cli_draft_wait" || operation.AttemptID != "" {
			t.Fatal("explicit CLI request did not wait for Draft readiness", operation, terminal)
		}
	} else if !terminal || operation.State != "held" || operation.Reason != "pull_draft" {
		t.Fatal("native Hosted draft hold lost its original reason", operation, terminal)
	}
	for _, table := range []string{"review_execution_reservations", "review_execution_evidence", "review_round_completions"} {
		if cliCount(t, f.store, `SELECT count(*) FROM `+table) != 0 {
			t.Fatal("draft spent capacity or credited a review", table)
		}
	}
	return operation
}

func TestDraftNativeDirectAndFinalAdmissionRefuseBothChannels(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			var f *cliDriverFixture
			var hosted *hostedDriverFixture
			var driver reviewexec.Driver
			if channel == "hosted" {
				hosted = newHostedDriverFixture(t)
				f, driver = hosted.cliDriverFixture, hosted.hosted
			} else {
				f = newCLIDriverFixture(t)
				driver = f.driver
			}
			setRuntimeDraft(f, true)
			raw, err := f.engine.Enqueue(t.Context(), f.scope(t, f.writer, false), "draft-direct", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: channel, HeadSHA: f.headSHA})
			if err != nil {
				t.Fatal(err)
			}
			var receipt reviewexec.Receipt
			if err := json.Unmarshal(raw, &receipt); err != nil {
				t.Fatal(err)
			}
			requested := waitRuntimeDraftState(t, f, driver, receipt.ID, channel)
			if channel == "cli" {
				if _, err := f.engine.ControlOperation(t.Context(), f.scope(t, f.writer, false), "cancel-draft-wait", requested.ID, reviewexec.OperationControl{ExpectedRevision: requested.Revision, Action: "cancel"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(f.log); !os.IsNotExist(err) || f.fetches.Load() != 0 || hosted != nil && (hosted.provider.queries != 0 || hosted.provider.reviews != 0) {
				t.Fatal("initial draft guard ran an allowance command, query or review", err)
			}
			// A fresh non-Draft preparation still cannot dispatch if the same
			// current comparison becomes Draft before the final action boundary.
			setRuntimeDraft(f, false)
			var operation reviewexec.Operation
			var attempt reviewexec.Attempt
			if hosted != nil {
				operation, attempt = hosted.prepareHosted(t)
			} else {
				operation, attempt = f.prepareAdmit(t, 2, 1, "ready-then-draft")
			}
			setRuntimeDraft(f, true)
			expectedReason := "source_changed"
			if channel == "hosted" {
				expectedReason = "source_admission_changed"
			}
			result, err := driver.Dispatch(t.Context(), operation, attempt, func(context.Context, string) error { t.Error("draft review was posted"); return nil })
			if err != nil || result.State != "failed" || result.Reason != expectedReason || hosted != nil && hosted.provider.reviews != 0 {
				t.Fatal("final source recheck admitted a Draft", result, err)
			}
			log, err := os.ReadFile(f.log)
			if err != nil && !os.IsNotExist(err) || strings.Contains(string(log), "review ") {
				t.Fatal("Draft started the CLI review process", err)
			}
			if cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_round_completions`) != 0 {
				t.Fatal("Draft final refusal received completion credit")
			}
		})
	}
}

func TestDraftHostedTimerRetryHoldsOriginalWithoutFreshProviderQuery(t *testing.T) {
	f, now := newHostedCooldownRetryFixture(t)
	op, first, config := f.startHosted(t)
	waiting := scheduleRuntimeCooldown(t, f, op, first, config)
	due, err := time.Parse(time.RFC3339Nano, waiting.Wait.RetryNotBefore)
	if err != nil {
		t.Fatal(err)
	}
	*now = due.Add(time.Millisecond)
	setRuntimeDraft(f.cliDriverFixture, true)
	held := waitRuntimeDraftState(t, f.cliDriverFixture, f.hosted, op.ID, "hosted")
	if held.ID != op.ID || held.AttemptID == first.ID || f.provider.queries != 1 || f.provider.reviews != 0 {
		t.Fatal("draft retry rebound intent or repeated provider work", held, f.provider.queries, f.provider.reviews)
	}
	if hostedDriverJournal(t, f, first.ID) != "held" {
		t.Fatal("Draft changed the original ended cooldown attempt")
	}
}

func TestDraftAfterHostedTriggerDoesNotCancelOriginalReview(t *testing.T) {
	f := newHostedDriverFixture(t)
	op, attempt := f.prepareHosted(t)
	confirmed := f.callback(t, &attempt)
	result, err := f.hosted.Dispatch(t.Context(), op, attempt, func(ctx context.Context, id string) error {
		if err := confirmed(ctx, id); err != nil {
			return err
		}
		setRuntimeDraft(f.cliDriverFixture, true)
		return nil
	})
	if err != nil || result.State != "completed" || f.provider.reviews != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 1 {
		t.Fatal("later Draft canceled or erased the already issued review", result, err)
	}
	if _, err := f.engine.Advance(t.Context(), f.project, attempt.ID, attempt.Revision, result.State, result.Reason, result.ExternalID); err != nil {
		t.Fatal(err)
	}
	if hostedDriverJournal(t, f, attempt.ID) != "completed" || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("already issued completion was not retained")
	}
}
