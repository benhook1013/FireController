package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestCLIDraftProbeReadFailuresAreBoundedAndSafe(t *testing.T) {
	for _, stage := range []string{"repository", "pull", "unknown-draft"} {
		t.Run(stage, func(t *testing.T) {
			f := supervisorFixture(t)
			now := time.Now()
			f.e.schedulingNow = func() time.Time { return now }
			r := f.enqueue(t, attemptProject, "draft-probe-failure", "cli", 2, 1)
			admissionSQL(t, f, `UPDATE review_operations SET reason=? WHERE id=?`, cliDraftWait, r.ID)
			var output bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
			defer slog.SetDefault(previous)
			reader := &refreshTestReader{}
			failure := &reviewprovider.Error{Code: "provider_unavailable", Message: "PRIVATE_PROVIDER_BODY"}
			if stage == "repository" {
				reader.repository = func(context.Context, int64) (reviewprovider.Repository, error) {
					return reviewprovider.Repository{}, failure
				}
			} else if stage == "pull" {
				reader.pull = func(context.Context, string, int64) (reviewprovider.Pull, error) {
					return reviewprovider.Pull{}, failure
				}
			} else {
				reader.pull = func(context.Context, string, int64) (reviewprovider.Pull, error) {
					p := lifecyclePull(2)
					p.IsDraft = nil
					return p, nil
				}
			}
			d := &draftWaitDriver{reader: reader}
			drivers := map[string]Driver{"test-capability": d}
			for range 2 {
				if err := f.e.processNext(t.Context(), drivers); err != nil {
					t.Fatal(err)
				}
			}
			text := output.String()
			providerStage := stage
			if stage == "unknown-draft" {
				providerStage = "pull"
				if !strings.Contains(text, `"reason":"draft_state_unknown"`) {
					t.Fatal("missing unknown Draft reason", text)
				}
			}
			if strings.Count(text, "CLI draft source probe failed") != 1 || !strings.Contains(text, r.ID) || !strings.Contains(text, `"provider_operation":"`+providerStage+`"`) || strings.Contains(text, "PRIVATE_PROVIDER_BODY") {
				t.Fatal("probe failure was silent, unbounded or exposed a provider body", text)
			}
			now = now.Add(cliDraftProbeInterval)
			if err := f.e.processNext(t.Context(), drivers); err != nil {
				t.Fatal(err)
			}
			if strings.Count(output.String(), "CLI draft source probe failed") != 2 {
				t.Fatal("next bounded probe failure was not observable")
			}
			if got := cliWaitRead(t, f, r.ID); got.State != "queued" || got.Reason != cliDraftWait || got.AttemptID != "" || d.prepares.Load() != 0 || d.calls.Load() != 0 {
				t.Fatal("provider failure changed intent or reached provider review", got)
			}
		})
	}
}

type draftHistoryQuery struct {
	EvidenceQuery
	second string
	calls  int
}

func (q *draftHistoryQuery) QueryRowContext(ctx context.Context, _ string, _ ...any) *sql.Row {
	q.calls++
	if q.calls == 1 {
		return q.EvidenceQuery.QueryRowContext(ctx, `SELECT 'cli_draft_wait'`)
	}
	return q.EvidenceQuery.QueryRowContext(ctx, q.second)
}

func TestHistoricalCLIDraftSeparatesLookupFailureFromInvalidHistory(t *testing.T) {
	f := supervisorFixture(t)
	o := Operation{ID: "operation", ProjectID: attemptProject, Channel: "cli"}
	a := Attempt{ID: "attempt", OperationID: o.ID, ProjectID: o.ProjectID, State: "held", Revision: 2}
	for _, tc := range []struct {
		name, query     string
		noRows, invalid bool
	}{
		{"missing", `SELECT 1 WHERE 0`, true, false},
		{"storage", `SELECT * FROM nonexistent_draft_history`, false, false},
		{"invalid-time", `SELECT 'invalid', 'invalid', 0`, false, true},
		{"occupied", `SELECT '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 1`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &draftHistoryQuery{EvidenceQuery: f.store.DB(), second: tc.query}
			_, draft, err := historicalCLIDraftOperation(t.Context(), q, o, a)
			if draft || err == nil {
				t.Fatal("invalid lookup became retained history", draft, err)
			}
			if tc.noRows && !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("missing-row identity was lost", err)
			}
			if tc.invalid && err.Error() != waitUnavailable().Error() {
				t.Fatal("invalid history lost its classification", err)
			}
			if tc.name == "storage" && !strings.Contains(err.Error(), "nonexistent_draft_history") {
				t.Fatal("storage error was rewritten as invalid history", err)
			}
		})
	}
}
