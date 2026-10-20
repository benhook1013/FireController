package reviewruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

func TestCLIDriverUsageFailureBecomesSafePreparationHold(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		want    string
	}{
		{"rate", &reviewprocess.Error{Code: "rate_limited"}, "allowance_exhausted"},
		{"wrapped rate", fmt.Errorf("private path or token: %w", &reviewprocess.Error{Code: "rate_limited"}), "allowance_exhausted"},
		{"usage", &reviewprocess.Error{Code: "usage_unavailable"}, "allowance_unavailable"},
		{"wrapped usage", fmt.Errorf("private path: %w", &reviewprocess.Error{Code: "usage_unavailable"}), "allowance_unavailable"},
		{"not started", &reviewprocess.Error{Code: "usage_unavailable", FailureStage: reviewprocess.FailureStageCommandNotStarted}, "allowance_command_not_started"},
		{"timeout", &reviewprocess.Error{Code: "usage_unavailable", FailureStage: reviewprocess.FailureStageTimeout}, "allowance_timeout"},
		{"command failed", &reviewprocess.Error{Code: "usage_unavailable", FailureStage: reviewprocess.FailureStageCommandFailed}, "allowance_command_failed"},
		{"wrapped malformed", fmt.Errorf("private token: %w", &reviewprocess.Error{Code: "usage_unavailable", FailureStage: reviewprocess.FailureStageResponseUnproven}), "allowance_response_unproven"},
		{"unknown stage", &reviewprocess.Error{Code: "usage_unavailable", FailureStage: "private_token"}, "allowance_unavailable"},
		{"unrelated code stage", &reviewprocess.Error{Code: "process_failed", FailureStage: reviewprocess.FailureStageTimeout}, "allowance_unavailable"},
		{"raw text", errors.New("rate_limited private token diagnostic"), "allowance_unavailable"},
		{"foreign typed code", core.Fail("rate_limited", "private diagnostic", 409), "allowance_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			var usage atomic.Int64
			f.driver.usage = func(context.Context, reviewworkspace.Workspace, string, string) (reviewprocess.Usage, error) {
				usage.Add(1)
				if tc.want == "allowance_exhausted" {
					return reviewprocess.Usage{Limit: 3, RollingWindowMS: 3600000}, tc.failure
				}
				return reviewprocess.Usage{}, tc.failure
			}
			op, a, c := f.start(t, 2, 1, "prepare-usage-refusal")
			prepared, err := f.driver.Prepare(t.Context(), op, c, a)
			var failure *core.Error
			if !errors.As(err, &failure) || failure.Code != "review_held" {
				t.Fatalf("usage error lost safe hold: %T %v", err, err)
			}
			details, ok := failure.Details.(map[string]string)
			if !ok || details["reason"] != tc.want || strings.Contains(failure.Error(), "private") || prepared.Anchor != (reviewexec.Anchor{}) {
				t.Fatal("bad safe refusal", failure, prepared)
			}
			wantRows := 0
			if tc.want == "allowance_exhausted" {
				wantRows = 1
			}
			if usage.Load() != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_cli_preflight`) != wantRows || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
				t.Fatal("refused allowance retained permission/evidence")
			}
			f.driver.mu.Lock()
			_, exists := f.driver.prepared[a.ID]
			f.driver.mu.Unlock()
			if exists {
				t.Fatal("failed preparation left dispatch entry")
			}
		})
	}
}

func TestCLIDriverRealUsageHoldSettlesSupervisorWithoutReview(t *testing.T) {
	for _, mode := range []string{"exhausted", "unavailable", "repository_changed", "not_started", "command_failed", "capture_failed", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			expected := "allowance_exhausted"
			if mode == "repository_changed" {
				f.reader.repository.ID = 999
				expected = "repository_changed"
			} else {
				body := `{"type":"status","phase":"usage","status":"complete","organization":"fixture-org","includedReviews":{"state":"available","repository":"owner/repo","remaining":0,"limit":3,"fullCapacityInMs":0,"rollingWindowMs":60000},"billingPeriod":{"state":"available","usageBillingStatus":"inactive"}}`
				if mode == "unavailable" {
					body = "private invalid provider diagnostic"
					expected = "allowance_response_unproven"
				}
				// The real Runner executes this allowlisted local Usage process. It emits no
				// network calls and would log any accidental review invocation.
				script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + cliShellQuote(f.log) + "\nprintf '%s\\n' " + cliShellQuote(body) + "\n"
				switch mode {
				case "not_started":
					expected = "allowance_command_not_started"
				case "command_failed":
					script = "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + cliShellQuote(f.log) + "\nprintf 'private failure' >&2\nexit 7\n"
					expected = "allowance_command_failed"
				case "capture_failed":
					script = "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + cliShellQuote(f.log) + "\nhead -c 65537 /dev/zero\n"
					expected = "allowance_command_failed"
				case "timeout":
					script = "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + cliShellQuote(f.log) + "\nexec sleep 60\n"
					expected = "allowance_timeout"
				}
				if err := os.WriteFile(f.capability.CLIExecutable, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "not_started" {
				if err := os.Remove(f.capability.CLIExecutable); err != nil {
					t.Fatal(err)
				}
			}
			var reviews atomic.Int64
			f.driver.review = func(context.Context, reviewworkspace.Workspace) (reviewprocess.Capture, error) {
				reviews.Add(1)
				return reviewprocess.Capture{}, errors.New("review must not execute")
			}
			raw, err := f.engine.Enqueue(t.Context(), f.scope(t, f.writer, false), "pipeline-preparation-reason", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: f.headSHA})
			if err != nil {
				t.Fatal(err)
			}
			var receipt reviewexec.Receipt
			if json.Unmarshal(raw, &receipt) != nil {
				t.Fatal(string(raw))
			}
			ctx, cancel := context.WithCancel(t.Context())
			done, err := f.engine.Start(ctx, map[string]reviewexec.Driver{f.capability.ID: f.driver})
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			var once sync.Once
			stop := func() {
				once.Do(func() {
					cancel()
					select {
					case err := <-done:
						if err != nil {
							t.Error(err)
						}
					case <-time.After(3 * time.Second):
						t.Error("supervisor did not stop")
					}
				})
			}
			defer stop()
			settleBudget := 4 * time.Second
			if mode == "timeout" {
				settleBudget = 20 * time.Second
			}
			deadline := time.NewTimer(settleBudget)
			defer deadline.Stop()
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			var operation reviewexec.Operation
			for {
				operation, err = f.engine.ReadOperation(t.Context(), f.scope(t, f.writer, false), receipt.ID)
				if err != nil {
					t.Fatal(err)
				}
				if operation.State == "held" {
					break
				}
				select {
				case <-deadline.C:
					t.Fatal("supervisor failed to settle refusal", operation)
				case <-tick.C:
				}
			}
			stop()
			if operation.Reason != expected || reviews.Load() != 0 || strings.Contains(operation.Reason, "private") {
				t.Fatal("safe hold reason missing or review dispatched", operation, reviews.Load())
			}
			wantRows := 0
			if mode == "exhausted" {
				wantRows = 1
				v := operation.Allowance
				if v == nil || v.Kind != "cli_exhausted" || v.Remaining == nil || *v.Remaining != 0 || v.Limit == nil || *v.Limit != 3 || v.FullCapacityInMS == nil || *v.FullCapacityInMS != 0 || v.RollingWindowMS == nil || *v.RollingWindowMS != 60000 {
					t.Fatal("native exhausted facts lost", v)
				}
			} else if operation.Allowance != nil {
				t.Fatal("unproven failure invented capacity", operation.Allowance)
			}
			if cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_cli_preflight`) != wantRows {
				t.Fatal("preparation hold retained permission/reservation/evidence")
			}
			var heldReason string
			if err := f.store.DB().QueryRow(`SELECT reason FROM review_execution_transitions WHERE attempt_id=? AND state='held'`, operation.AttemptID).Scan(&heldReason); err != nil || heldReason != expected {
				t.Fatal("held reason not journaled", heldReason, err)
			}
			log, err := os.ReadFile(f.log)
			if mode == "not_started" {
				if !os.IsNotExist(err) {
					t.Fatal("unstarted command ran", err)
				}
			} else if mode == "repository_changed" {
				if !os.IsNotExist(err) || f.fetches.Load() != 0 {
					t.Fatal("repository mismatch reached allowance/fetch", err, string(log))
				}
			} else if err != nil || string(log) != "usage --agent\n" {
				t.Fatal("usage repeated/review invoked", string(log), err)
			}
		})
	}
}
