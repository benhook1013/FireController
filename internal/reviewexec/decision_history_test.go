package reviewexec

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

func TestSelectionDecisionHistoryCompactionIsolationRetention(t *testing.T) {
	f := selectionFixture(t)
	now := time.Now().UTC()
	write := func(project, channel string, d SelectionDecision, at time.Time) {
		t.Helper()
		if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			return writeSelectionDecision(ctx, tx, project, channel, d, at)
		}); err != nil {
			t.Fatal(err)
		}
	}
	d := SelectionDecision{Phase: "automatic_plan", Outcome: "no_new_intent", Reason: "lane_busy", Candidates: []SelectionCandidateDecision{}}
	write(attemptProject, "cli", d, now)
	write(attemptProject, "cli", d, now.Add(time.Second))
	read := func(project, channel string, before int64, limit int) SelectionDecisionHistory {
		t.Helper()
		out, err := f.e.ReadSelectionDecisionHistory(t.Context(), selectionScope(t, f, project, f.reader, "read"), channel, before, limit)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := read(attemptProject, "cli", 0, 10)
	if len(out.Items) != 1 || out.Items[0].Occurrences != 2 || out.Items[0].FirstSeenAt == out.Items[0].LastSeenAt {
		t.Fatal(out)
	}
	write(attemptProject, "hosted", d, now)
	if len(read(attemptProject, "cli", 0, 10).Items) != 1 {
		t.Fatal("channel leak")
	}
	for i := 0; i < decisionHistoryLimit+10; i++ {
		next := d
		next.Reason = fmt.Sprintf("reason_%d", i)
		write(attemptProject, "cli", next, now.Add(2*time.Second))
	}
	var count int
	f.store.DB().QueryRow("SELECT count(*) FROM review_selection_decision_history WHERE project_id=? AND channel='cli'", attemptProject).Scan(&count)
	if count != decisionHistoryLimit {
		t.Fatal(count)
	}
	page := read(attemptProject, "cli", 0, 3)
	if len(page.Items) != 3 || !page.HasMore {
		t.Fatal(page)
	}
	second := read(attemptProject, "cli", page.Items[2].Sequence, 3)
	if second.Items[0].Sequence >= page.Items[2].Sequence {
		t.Fatal(second)
	}
	write(attemptProject, "cli", d, now.Add(8*24*time.Hour))
	f.store.DB().QueryRow("SELECT count(*) FROM review_selection_decision_history WHERE project_id=? AND channel='cli'", attemptProject).Scan(&count)
	if count != 1 {
		t.Fatal("age retention", count)
	}
	if _, err := f.e.ReadSelectionDecisionHistory(t.Context(), selectionScope(t, f, attemptProject, f.reader, "read"), "other", 0, 10); err == nil {
		t.Fatal("invalid channel")
	}
}
func TestDecisionTraceCapturesRealCandidateReplacement(t *testing.T) {
	trace := &decisionTrace{}
	ctx := context.WithValue(t.Context(), decisionTraceKey{}, trace)
	traceDecision(ctx, 2, "", "unknown", "source_pending", 0)
	traceDecision(ctx, 2, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "held", "predecessor_unfinished", 1)
	traceDecision(ctx, 4, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "ready", "", 0)
	if len(trace.candidates) != 2 || trace.candidates[0].HeadSHA == "" || trace.candidates[0].BlockingPullNumber != 1 || trace.candidates[1].Outcome != "ready" {
		t.Fatal(trace)
	}
}

func TestSelectionDecisionHistoryRealSettlementAndReadIsolation(t *testing.T) {
	f := selectionFixture(t)
	receipt, _ := selectionEnqueue(t, f, "history-selected", "cli", nil)
	selectionNext(t, f, &refreshTestReader{})
	scope := selectionScope(t, f, attemptProject, f.reader, "read")
	before := selectionCounts(t, f)
	got, err := f.e.ReadSelectionDecisionHistory(t.Context(), scope, "cli", 0, 50)
	if err != nil || len(got.Items) != 1 {
		t.Fatal(got, err)
	}
	d := got.Items[0]
	if d.Outcome != "selected" || d.SelectedPullNumber != 2 || d.SelectionID != receipt.ID || len(d.Candidates) != 1 || d.Candidates[0].HeadSHA == "" || d.Candidates[0].Outcome != "ready" {
		t.Fatal(d)
	}
	other, err := f.e.ReadSelectionDecisionHistory(t.Context(), selectionScope(t, f, otherAttemptProject, f.other, "read"), "cli", 0, 50)
	if err != nil || len(other.Items) != 0 {
		t.Fatal("project isolation", other, err)
	}
	if selectionCounts(t, f) != before {
		t.Fatal("history read changed scheduler")
	}
}

func TestDecisionOmissionBufferBoundedAndScoped(t *testing.T) {
	var b decisionOmissionBuffer
	b.add("one", "hosted")
	for i := 0; i < 300; i++ {
		b.add("two", "cli")
	}
	if b.count("one", "hosted") != 0 || b.count("two", "cli") != 256 || b.count("two", "hosted") != 0 {
		t.Fatal("omission ring scope or bound")
	}
}

func TestDecisionHistoryStorageFailureDoesNotBlockSelection(t *testing.T) {
	f := selectionFixture(t)
	if _, err := f.store.DB().Exec(`CREATE TRIGGER fail_decision_history BEFORE INSERT ON review_selection_decision_history BEGIN SELECT RAISE(ABORT,'history-only failure'); END`); err != nil {
		t.Fatal(err)
	}
	receipt, _ := selectionEnqueue(t, f, "history-failure", "cli", nil)
	selectionNext(t, f, &refreshTestReader{})
	got := selectionRead(t, f, receipt.ID)
	if got.State != "selected" {
		t.Fatal(got)
	}
	history, err := f.e.ReadSelectionDecisionHistory(t.Context(), selectionScope(t, f, attemptProject, f.reader, "read"), "cli", 0, 50)
	if err != nil || len(history.Items) != 0 || history.OmittedRecentPasses != 1 {
		t.Fatal(history, err)
	}
}

func TestDecisionHistoryBackwardClockStartsNewInterval(t *testing.T) {
	f := selectionFixture(t)
	now := time.Now().UTC()
	d := SelectionDecision{Phase: "automatic_plan", Outcome: "no_new_intent"}
	for _, at := range []time.Time{now, now.Add(-time.Second)} {
		if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			return writeSelectionDecision(ctx, tx, attemptProject, "cli", d, at)
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.e.ReadSelectionDecisionHistory(t.Context(), selectionScope(t, f, attemptProject, f.reader, "read"), "cli", 0, 50)
	if err != nil || len(got.Items) != 2 || got.Items[0].Occurrences != 1 {
		t.Fatal(got, err)
	}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := ValidateDecisionHistorySnapshot(t.Context(), tx); err != nil {
		t.Fatal("backup validation after backward clock", err)
	}
}
