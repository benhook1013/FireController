package reviewruntime

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

func configureExplicitStack(t *testing.T, f *cliDriverFixture) {
	t.Helper()
	parent := f.reader.pulls[3]
	parent.HeadSHA = f.baseSHA
	parent.HeadRef = "synthetic-parent"
	f.reader.pulls[3] = parent
	target := f.reader.pulls[4]
	target.BaseSHA, target.BaseRef = parent.HeadSHA, "synthetic-parent"
	f.reader.pulls[4] = target
	f.configure(t, "stacked", []int64{2, 3, 4})
}

func startExplicitOperation(t *testing.T, f *cliDriverFixture, channel, key string) (reviewexec.Operation, reviewexec.Attempt, reviewexec.Configuration) {
	t.Helper()
	raw, err := f.engine.Enqueue(t.Context(), f.scope(t, f.writer, false), key, reviewexec.Request{ExpectedConfigurationRevision: 2, PullNumber: 4, Channel: channel, HeadSHA: f.reader.pulls[4].HeadSHA, AllowOutOfOrder: true})
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	if err = json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	attempt, err := f.engine.StartAttempt(t.Context(), f.project, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.engine.ExecutionContext(t.Context(), f.project, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Operation.AllowOutOfOrder {
		t.Fatal("explicit queue-order choice was not retained")
	}
	return current.Operation, attempt, current.Configuration
}

func TestExplicitOrderSkipsOnlyPredecessorCompletionTraversal(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "explicit"}[explicit], func(t *testing.T) {
			f := newCLIDriverFixture(t)
			configureExplicitStack(t, f)
			operation := sourceOperation(f, 4, "cli")
			operation.ConfigurationRevision = 2
			operation.AllowOutOfOrder = explicit
			plan, predecessors, err := resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, sourceConfiguration("stacked", 2, 3, 4), operation)
			if !explicit {
				sourceHoldReason(t, err, "predecessor_unfinished")
				if plan.Pull.Number != 4 || predecessors != nil || f.reader.calls[3] != 1 || f.reader.calls[2] != 0 {
					t.Fatal("ordinary request did not check its actual parent only", plan, predecessors, f.reader.calls)
				}
				return
			}
			if err != nil || plan.Pull.Number != 4 || plan.ParentPullNumber != 3 || plan.ParentSHA != f.baseSHA || predecessors != nil {
				t.Fatal("explicit request changed target comparison or returned a predecessor witness", plan, predecessors, err)
			}
			if f.reader.calls[4] != 1 || f.reader.calls[3] != 1 || f.reader.calls[2] != 0 {
				t.Fatal("explicit request did not limit reads to target and effective parent", f.reader.calls)
			}
			request := operationRequest(operation)
			if !request.AllowOutOfOrder {
				t.Fatal("operation request dropped explicit ordering intent")
			}
		})
	}
}

func TestExplicitOrderRunsSyntheticCLIAndHostedDrivers(t *testing.T) {
	t.Run("cli", func(t *testing.T) {
		f := newCLIDriverFixture(t)
		configureExplicitStack(t, f)
		op, attempt, configuration := startExplicitOperation(t, f, "cli", "explicit-cli")
		prepared, err := f.driver.Prepare(t.Context(), op, configuration, attempt)
		if err != nil || prepared.Anchor.ParentPullNumber != 3 || prepared.Anchor.ParentSHA != f.baseSHA {
			t.Fatal("explicit CLI preparation lost effective parent", prepared, err)
		}
		attempt, err = f.engine.Admit(t.Context(), f.project, attempt.ID, attempt.Revision, prepared.Anchor, prepared.Intent)
		if err != nil {
			t.Fatal(err)
		}
		result, err := f.driver.Dispatch(t.Context(), op, attempt, nil)
		if err != nil || result.State != "completed" {
			t.Fatal("explicit CLI target did not run", result, err)
		}
		if f.reader.calls[2] != 0 || f.reader.calls[3] != 2 || f.reader.calls[4] != 2 {
			t.Fatal("CLI inspected the completion prefix or lost fresh target/parent reads", f.reader.calls)
		}
	})

	t.Run("hosted", func(t *testing.T) {
		f := newHostedDriverFixture(t)
		configureExplicitStack(t, f.cliDriverFixture)
		f.provider.pullNumber = 4
		op, attempt, configuration := startExplicitOperation(t, f.cliDriverFixture, "hosted", "explicit-hosted")
		prepared, err := f.hosted.Prepare(t.Context(), op, configuration, attempt)
		if err != nil || prepared.Anchor.ParentPullNumber != 3 || prepared.Anchor.ParentSHA != f.baseSHA {
			t.Fatal("explicit Hosted preparation lost effective parent", prepared, err)
		}
		attempt, err = f.engine.Admit(t.Context(), f.project, attempt.ID, attempt.Revision, prepared.Anchor, prepared.Intent)
		if err != nil {
			t.Fatal(err)
		}
		result, err := f.hosted.Dispatch(t.Context(), op, attempt, f.callback(t, &attempt))
		if err != nil || result.State != "completed" || f.provider.reviews != 1 {
			t.Fatal("explicit Hosted target did not run", result, err, f.provider.reviews)
		}
		// Hosted refreshes exact target/parent after Git materialization, then
		// again at dispatch. It must never inspect the unrelated queue prefix.
		if f.reader.calls[2] != 0 || f.reader.calls[3] != 3 || f.reader.calls[4] != 3 {
			t.Fatal("Hosted inspected the completion prefix or lost fresh target/parent reads", f.reader.calls)
		}
	})
}

func TestExplicitOrderFreshParentMovementRefusesBeforeAction(t *testing.T) {
	t.Run("cli", func(t *testing.T) {
		f := newCLIDriverFixture(t)
		configureExplicitStack(t, f)
		op, attempt, configuration := startExplicitOperation(t, f, "cli", "explicit-cli-movement")
		prepared, err := f.driver.Prepare(t.Context(), op, configuration, attempt)
		if err != nil {
			t.Fatal(err)
		}
		attempt, err = f.engine.Admit(t.Context(), f.project, attempt.ID, attempt.Revision, prepared.Anchor, prepared.Intent)
		if err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(f.log)
		if err != nil {
			t.Fatal(err)
		}
		parent := f.reader.pulls[3]
		parent.HeadSHA = strings.Repeat("9", 40)
		f.reader.pulls[3] = parent
		result, err := f.driver.Dispatch(t.Context(), op, attempt, nil)
		after, readErr := os.ReadFile(f.log)
		if err != nil || result.State != "failed" || readErr != nil || string(after) != string(before) {
			t.Fatal("moved parent reached CLI review process", result, err, readErr)
		}
	})

	t.Run("hosted", func(t *testing.T) {
		f := newHostedDriverFixture(t)
		configureExplicitStack(t, f.cliDriverFixture)
		f.provider.pullNumber = 4
		op, attempt, configuration := startExplicitOperation(t, f.cliDriverFixture, "hosted", "explicit-hosted-movement")
		prepared, err := f.hosted.Prepare(t.Context(), op, configuration, attempt)
		if err != nil {
			t.Fatal(err)
		}
		attempt, err = f.engine.Admit(t.Context(), f.project, attempt.ID, attempt.Revision, prepared.Anchor, prepared.Intent)
		if err != nil {
			t.Fatal(err)
		}
		parent := f.reader.pulls[3]
		parent.HeadSHA = strings.Repeat("9", 40)
		f.reader.pulls[3] = parent
		result, err := f.hosted.Dispatch(t.Context(), op, attempt, f.callback(t, &attempt))
		if err != nil || result.State != "failed" || f.provider.reviews != 0 {
			t.Fatal("moved parent reached Hosted post", result, err, f.provider.reviews)
		}
	})
}

func TestExplicitOrderTamperAndTargetPendingStillRefuse(t *testing.T) {
	f := newCLIDriverFixture(t)
	configureExplicitStack(t, f)
	op, attempt, configuration := startExplicitOperation(t, f, "cli", "explicit-tamper")
	prepared, err := f.driver.Prepare(t.Context(), op, configuration, attempt)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = f.engine.Admit(t.Context(), f.project, attempt.ID, attempt.Revision, prepared.Anchor, prepared.Intent)
	if err != nil {
		t.Fatal(err)
	}
	tampered := op
	tampered.AllowOutOfOrder = false
	before, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.driver.Dispatch(t.Context(), tampered, attempt, nil)
	after, readErr := os.ReadFile(f.log)
	if err != nil || result.State != "failed" || result.Reason != "execution_binding_changed" || readErr != nil || string(after) != string(before) {
		t.Fatal("tampered ordering intent reached review process", result, err, readErr)
	}

	// Create genuine pending target evidence through a separate explicit run.
	f = newCLIDriverFixture(t)
	configureExplicitStack(t, f)
	op, attempt, configuration = startExplicitOperation(t, f, "cli", "explicit-pending-seed")
	prepared, err = f.driver.Prepare(t.Context(), op, configuration, attempt)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = f.engine.Admit(t.Context(), f.project, attempt.ID, attempt.Revision, prepared.Anchor, prepared.Intent)
	if err != nil {
		t.Fatal(err)
	}
	result, err = f.driver.Dispatch(t.Context(), op, attempt, nil)
	if err != nil || result.State != "completed" {
		t.Fatal(result, err)
	}
	if _, err = f.engine.Advance(context.Background(), f.project, attempt.ID, attempt.Revision, result.State, result.Reason, ""); err != nil {
		t.Fatal(err)
	}
	next := op
	next.ID = "review_operation_" + strings.Repeat("e", 32)
	_, _, err = resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, configuration, next)
	assertCoreErrorCode(t, err, "review_evidence_pending")
}
