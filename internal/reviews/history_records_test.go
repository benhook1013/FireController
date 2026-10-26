package reviews

import (
	"testing"
)

func TestHistoryRunDetailsVersionTogetherWithoutCopyingUnrelatedRecords(t *testing.T) {
	f := newHistoryWriterFixture(t)
	tx := historyWriterTx(t, f)
	run := RunInput{SourceRunID: "external-run", Channel: "cli", HeadSHA: "head", Scope: "full", Outcome: "completed", Complete: true, Attributable: true, Found: 1, Findings: []Finding{{SourceFindingID: "finding-1", Title: "Original title", Detail: "Original body", Disposition: "unadjudicated"}}}
	first := historyWriterSnapshot(t, tx, f.project, 1)
	one := newHistorySnapshotWrite(f.project, 1)
	if err := one.pull(t.Context(), tx, 7, 0, []any{"Owning PR", "open", "head", "main", "base", "unknown", "unknown", false, 0, 0, "[]", "[]", "{}", int64(1)}); err != nil {
		t.Fatal(err)
	}
	if err := one.run(t.Context(), tx, "imported", 7, 0, run); err != nil {
		t.Fatal(err)
	}
	if err := one.applyCurrent(t.Context(), tx, first, 0); err != nil {
		t.Fatal(err)
	}
	second := historyWriterSnapshot(t, tx, f.project, 2)
	two := newHistorySnapshotWrite(f.project, 2)
	if err := two.run(t.Context(), tx, "imported", 7, 1, run); err != nil {
		t.Fatal(err)
	}
	if err := two.attempt(t.Context(), tx, "native-executor", 7, 0, Attempt{SourceAttemptID: "attempt-1", Channel: "cli", HeadSHA: "head", State: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := two.applyCurrent(t.Context(), tx, second, 1); err != nil {
		t.Fatal(err)
	}
	third := historyWriterSnapshot(t, tx, f.project, 3)
	three := newHistorySnapshotWrite(f.project, 3)
	// Changing decision text without changing summary counters must still
	// append an exact run/detail version, leaving the first snapshot intact.
	run.Findings[0].DecisionReason = "New adjudication note"
	if err := three.run(t.Context(), tx, "imported", 7, 1, run); err != nil {
		t.Fatal(err)
	}
	if err := three.applyCurrent(t.Context(), tx, third, 2); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"review_run_versions": 2, "review_finding_versions": 2, "review_attempt_versions": 1} {
		var count int
		if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s: %d want%d: %v", table, count, want, err)
		}
	}
	var original, corrected string
	if err := tx.QueryRowContext(t.Context(), "SELECT decision_reason FROM review_finding_versions WHERE run_version_revision=1").Scan(&original); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(t.Context(), "SELECT decision_reason FROM review_finding_versions WHERE run_version_revision=3").Scan(&corrected); err != nil {
		t.Fatal(err)
	}
	if original != "" || corrected != run.Findings[0].DecisionReason {
		t.Fatal("new decision overwrote retained detail")
	}
	if err := ValidateHistoryReferences(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryRecordIdentitiesSurviveOmissionAndCannotBeRebound(t *testing.T) {
	f := newHistoryWriterFixture(t)
	tx := historyWriterTx(t, f)
	run := RunInput{SourceRunID: "same-id", Channel: "hosted", HeadSHA: "old-head", Scope: "full", Findings: []Finding{}}
	attempt := Attempt{SourceAttemptID: "same-id", Channel: "cli", HeadSHA: "old-head", State: "completed", TriggerID: "trigger-1"}
	first := historyWriterSnapshot(t, tx, f.project, 1)
	one := newHistorySnapshotWrite(f.project, 1)
	if err := one.pull(t.Context(), tx, 7, 0, []any{"Owning PR", "open", "head", "main", "base", "unknown", "unknown", false, 0, 0, "[]", "[]", "{}", int64(1)}); err != nil {
		t.Fatal(err)
	}
	if err := one.run(t.Context(), tx, "first-producer", 7, 0, run); err != nil {
		t.Fatal(err)
	}
	if err := one.attempt(t.Context(), tx, "first-producer", 7, 0, attempt); err != nil {
		t.Fatal(err)
	}
	// Another original producer legitimately has the same external identities.
	if err := one.run(t.Context(), tx, "second-producer", 7, 1, run); err != nil {
		t.Fatal(err)
	}
	if err := one.attempt(t.Context(), tx, "second-producer", 7, 1, attempt); err != nil {
		t.Fatal(err)
	}
	if err := one.applyCurrent(t.Context(), tx, first, 0); err != nil {
		t.Fatal(err)
	}
	second := historyWriterSnapshot(t, tx, f.project, 2)
	two := newHistorySnapshotWrite(f.project, 2)
	id := runOpaqueID(f.project, "first-producer", run.SourceRunID)
	if err := two.remove(t.Context(), tx, historyRunRecord, []any{id}); err != nil {
		t.Fatal(err)
	}
	if err := two.remove(t.Context(), tx, historyAttemptRecord, []any{"first-producer", attempt.SourceAttemptID}); err != nil {
		t.Fatal(err)
	}
	if err := two.applyCurrent(t.Context(), tx, second, 1); err != nil {
		t.Fatal(err)
	}
	_ = historyWriterSnapshot(t, tx, f.project, 3)
	three := newHistorySnapshotWrite(f.project, 3)
	nativeErrorCode(t, three.run(t.Context(), tx, "first-producer", 8, 0, run), "observation_conflict")
	moved := run
	moved.HeadSHA = "new-head"
	nativeErrorCode(t, three.run(t.Context(), tx, "first-producer", 7, 0, moved), "observation_conflict")
	nativeErrorCode(t, three.attempt(t.Context(), tx, "first-producer", 8, 0, attempt), "observation_conflict")
	reopened := attempt
	reopened.State = "running"
	nativeErrorCode(t, three.attempt(t.Context(), tx, "first-producer", 7, 0, reopened), "observation_conflict")
	erased := attempt
	erased.TriggerID = ""
	nativeErrorCode(t, three.attempt(t.Context(), tx, "first-producer", 7, 0, erased), "observation_conflict")
	if err := three.run(t.Context(), tx, "first-producer", 7, 0, run); err != nil {
		t.Fatal("returning same source should reuse old body", err)
	}
}
