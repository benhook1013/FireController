package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func TestCLIWaitHistoryIsolatesAccountAndChannel(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			f, a, _, now := cliWaitFixture(t)
			f.configure(t, otherAttemptProject, "other-account", 0, false)
			if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
				t.Fatal(err)
			}
			due := cliWaitDue(t, cliWaitRead(t, f, a.OperationID))
			if cancelled {
				timerControl(t, f, a.OperationID, "cancel", "isolation-cancel")
			}
			bad := f.enqueue(t, attemptProject, "same-account", "cli", 4, 1)
			hosted := f.enqueue(t, attemptProject, "same-account-hosted", "hosted", 2, 1)
			healthy := f.enqueue(t, otherAttemptProject, "other-account-cli", "cli", 2, 1)
			admissionSQL(t, f, `UPDATE review_cli_preflight SET body_json='{}' WHERE attempt_id=?`, a.ID)
			*now = due.Add(-time.Second)
			tables := []string{"review_operations", "review_execution_attempts", "review_operation_waits", "review_cli_preflight", "review_execution_reservations", "events"}
			before := map[string]string{}
			for _, table := range tables {
				before[table] = snapshotTable(t, f, table)
			}
			for range 2 {
				pending, err := f.e.pending(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				got := map[string]bool{}
				for _, o := range pending {
					got[o.ID] = true
				}
				if got[bad.ID] || got[a.OperationID] || !got[hosted.ID] || !got[healthy.ID] || len(got) != 2 {
					t.Fatal("account/channel isolation failed", got)
				}
			}
			for _, table := range tables {
				if before[table] != snapshotTable(t, f, table) {
					t.Fatal("pending mutated history", table)
				}
			}
			if got, err := f.e.StartAttempt(t.Context(), attemptProject, bad.ID); got.ID != "" || !isWaitUnavailable(err) {
				t.Fatal("corrupt account admitted", got, err)
			}
			f.start(t, attemptProject, hosted)
			f.start(t, otherAttemptProject, healthy)
			*now = due
			pending, err := f.e.pending(t.Context())
			if err != nil || cancelled && (len(pending) != 1 || pending[0].ID != bad.ID) || !cancelled && len(pending) != 0 {
				t.Fatal("expiry did not preserve account and predecessor boundaries", pending, err)
			}
			assertCLIWaitNoCredit(t, f)
		})
	}
}

func TestCLIWaitIsolationIsTypedAndBounded(t *testing.T) {
	now := time.Now()
	e := &Engine{schedulingNow: func() time.Time { return now }}
	o := Operation{ProjectID: "project", Channel: "cli"}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, sql.ErrTxDone, errors.New("database failure"), core.Fail("storage_unavailable", "Stored review waiting intent is unavailable.", 500)} {
		if e.skipUnavailableCLIWait(t.Context(), o, err) {
			t.Fatal("operational error suppressed", err)
		}
	}
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	for i := range 300 {
		o.ID = fmt.Sprintf("review_operation_%d", i)
		for range 2 {
			if !e.skipUnavailableCLIWait(t.Context(), o, fmt.Errorf("private data: %w", waitUnavailable())) {
				t.Fatal("typed refusal escaped")
			}
		}
	}
	if len(e.configurationFailureNext) != 256 || strings.Count(output.String(), "\n") != 256 || strings.Contains(output.String(), "private data") {
		t.Fatal("unbounded diagnostic")
	}
	now = now.Add(30 * time.Second)
	if !e.skipUnavailableCLIWait(t.Context(), o, waitUnavailable()) || len(e.configurationFailureNext) != 1 {
		t.Fatal("diagnostic did not expire")
	}
	o.Channel = "hosted"
	if e.skipUnavailableCLIWait(t.Context(), o, waitUnavailable()) {
		t.Fatal("changed Hosted validation behavior")
	}
}
