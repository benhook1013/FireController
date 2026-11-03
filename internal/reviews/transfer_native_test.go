package reviews

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

// These tests use the real retained writer and SQLite. The optional executor
// tables below are identity-probe fixtures, not fabricated native authority or
// evidence that imports can satisfy executor coverage.
func transferNativeReadOnly(t *testing.T, tx *sql.Tx, call func() error) error {
	t.Helper()
	var changes, schema int64
	if err := tx.QueryRow(`SELECT total_changes()`).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(`PRAGMA schema_version`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	err := call()
	var afterChanges, afterSchema int64
	if err := tx.QueryRow(`SELECT total_changes()`).Scan(&afterChanges); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(`PRAGMA schema_version`).Scan(&afterSchema); err != nil {
		t.Fatal(err)
	}
	if changes != afterChanges || schema != afterSchema {
		t.Fatal("native transfer guard changed rows or schema")
	}
	return err
}

func transferNativeCloneRun(t *testing.T, run RunInput) RunInput {
	t.Helper()
	body, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	var copy RunInput
	if err := json.Unmarshal(body, &copy); err != nil {
		t.Fatal(err)
	}
	return copy
}

func TestTransferNativeRetainedExactBodiesSurviveOmission(t *testing.T) {
	f := newHistoryWriterFixture(t)
	tx := historyWriterTx(t, f)
	run := nativeRun("review_evidence_11111111111111111111111111111111")
	run.Findings[0].Route = nativeRouteFixture(0)
	run.Findings = append(run.Findings, nativeRun("second-finding").Findings[0])
	run.Found, run.Accepted = 2, 2
	duration := int64(60)
	attempt := nativeAttempt("review_attempt_11111111111111111111111111111111", "completed")
	attempt.SourceRunID, attempt.FinishedAt, attempt.DurationSeconds = run.SourceRunID, run.FinishedAt, &duration
	attempt.TriggerID, attempt.ProviderReviewID = "trigger", "provider-review"
	first := historyWriterSnapshot(t, tx, f.project, 1)
	w := newHistorySnapshotWrite(f.project, 1)
	if err := w.pull(t.Context(), tx, 7, 0, []any{"Owning PR", "open", "head", "main", "base", "unknown", "unknown", false, 0, 0, "[]", "[]", "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := w.run(t.Context(), tx, nativeSource, 7, 0, run); err != nil {
		t.Fatal(err)
	}
	if err := w.attempt(t.Context(), tx, nativeSource, 7, 0, attempt); err != nil {
		t.Fatal(err)
	}
	if err := w.applyCurrent(t.Context(), tx, first, 0); err != nil {
		t.Fatal(err)
	}
	second := historyWriterSnapshot(t, tx, f.project, 2)
	w = newHistorySnapshotWrite(f.project, 2)
	if err := w.remove(t.Context(), tx, historyRunRecord, []any{runOpaqueID(f.project, nativeSource, run.SourceRunID)}); err != nil {
		t.Fatal(err)
	}
	if err := w.remove(t.Context(), tx, historyAttemptRecord, []any{nativeSource, attempt.SourceAttemptID}); err != nil {
		t.Fatal(err)
	}
	if err := w.applyCurrent(t.Context(), tx, second, 1); err != nil {
		t.Fatal(err)
	}
	if err := ValidateHistoryReferences(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	checkRun := func(value RunInput) error {
		return transferNativeReadOnly(t, tx, func() error { return transferNativeRunGuard(t.Context(), tx, f.project, nativeSource, 7, value) })
	}
	checkAttempt := func(value Attempt) error {
		return transferNativeReadOnly(t, tx, func() error { return transferNativeAttemptGuard(t.Context(), tx, f.project, nativeSource, 7, value) })
	}
	if err := checkRun(run); err != nil {
		t.Fatal("exact omitted run refused", err)
	}
	if err := checkAttempt(attempt); err != nil {
		t.Fatal("exact omitted attempt refused", err)
	}
	// Every scalar body field is protected, including mutable display fields
	// that ordinary observation updates deliberately allow to evolve.
	for i := 0; i < reflect.TypeOf(run).NumField(); i++ {
		field := reflect.TypeOf(run).Field(i)
		if field.Name == "SourceRunID" || field.Name == "Findings" || field.Name == "CoverageLimits" {
			continue
		}
		t.Run("run/"+field.Name, func(t *testing.T) {
			changed := transferNativeCloneRun(t, run)
			transferNativeChangeScalar(t, reflect.ValueOf(&changed).Elem().Field(i))
			nativeErrorCode(t, checkRun(changed), "observation_conflict")
		})
	}
	t.Run("run/coverage", func(t *testing.T) {
		changed := transferNativeCloneRun(t, run)
		changed.CoverageLimits = append(changed.CoverageLimits, "Extra imported detail")
		nativeErrorCode(t, checkRun(changed), "observation_conflict")
	})
	for _, change := range []string{"reorder", "remove", "append"} {
		t.Run("finding/"+change, func(t *testing.T) {
			changed := transferNativeCloneRun(t, run)
			switch change {
			case "reorder":
				changed.Findings[0], changed.Findings[1] = changed.Findings[1], changed.Findings[0]
			case "remove":
				changed.Findings = changed.Findings[:1]
			case "append":
				changed.Findings = append(changed.Findings, nativeRun("additional-finding").Findings[0])
			}
			nativeErrorCode(t, checkRun(changed), "observation_conflict")
		})
	}
	for i := 0; i < reflect.TypeOf(run.Findings[0]).NumField(); i++ {
		field := reflect.TypeOf(run.Findings[0]).Field(i)
		t.Run("finding/"+field.Name, func(t *testing.T) {
			changed := transferNativeCloneRun(t, run)
			if field.Name == "Route" {
				changed.Findings[0].Route.ProofNote += " imported"
			} else {
				transferNativeChangeScalar(t, reflect.ValueOf(&changed.Findings[0]).Elem().Field(i))
			}
			nativeErrorCode(t, checkRun(changed), "observation_conflict")
		})
	}
	for i := 0; i < reflect.TypeOf(attempt).NumField(); i++ {
		field := reflect.TypeOf(attempt).Field(i)
		if field.Name == "SourceAttemptID" {
			continue
		}
		t.Run("attempt/"+field.Name, func(t *testing.T) {
			changed := attempt
			if field.Name == "DurationSeconds" {
				changed.DurationSeconds = nil
			} else {
				transferNativeChangeScalar(t, reflect.ValueOf(&changed).Elem().Field(i))
			}
			nativeErrorCode(t, checkAttempt(changed), "observation_conflict")
		})
	}
	// New independent native-labelled identities remain importable display
	// records when no local journal owns them. Other producers are separate.
	newRun := transferNativeCloneRun(t, run)
	newRun.SourceRunID = "other-external-evidence"
	if err := checkRun(newRun); err != nil {
		t.Fatal("absent native display identity refused", err)
	}
	newAttempt := attempt
	newAttempt.SourceAttemptID = "other-external-attempt"
	if err := checkAttempt(newAttempt); err != nil {
		t.Fatal("absent native display attempt refused", err)
	}
	// Even an exact retained display replay must refuse when an actual local
	// authority table owns its identity; equality does not authorize import.
	for _, ddl := range []string{
		`CREATE TABLE review_execution_evidence(id TEXT PRIMARY KEY,project_id TEXT NOT NULL)`,
		`CREATE TABLE review_execution_attempts(id TEXT PRIMARY KEY,project_id TEXT NOT NULL)`,
	} {
		if _, err := tx.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO review_execution_evidence VALUES(?,?)`, run.SourceRunID, f.project); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO review_execution_attempts VALUES(?,?)`, attempt.SourceAttemptID, f.project); err != nil {
		t.Fatal(err)
	}
	nativeErrorCode(t, checkRun(run), "observation_conflict")
	nativeErrorCode(t, checkAttempt(attempt), "observation_conflict")
}

func transferNativeChangeScalar(t *testing.T, value reflect.Value) {
	t.Helper()
	switch value.Kind() {
	case reflect.String:
		value.SetString(value.String() + " changed")
	case reflect.Bool:
		value.SetBool(!value.Bool())
	case reflect.Int64:
		value.SetInt(value.Int() + 1)
	default:
		t.Fatalf("unhandled scalar kind %s", value.Kind())
	}
}

func TestTransferNativeLocalAuthorityCollisionsAndProjectScope(t *testing.T) {
	f := newHistoryWriterFixture(t)
	tx := historyWriterTx(t, f)
	for _, ddl := range []string{
		`CREATE TABLE review_execution_evidence(id TEXT PRIMARY KEY,project_id TEXT NOT NULL)`,
		`CREATE TABLE review_execution_attempts(id TEXT PRIMARY KEY,project_id TEXT NOT NULL,state TEXT NOT NULL)`,
		`CREATE TABLE review_operations(id TEXT PRIMARY KEY,project_id TEXT NOT NULL,attempt_id TEXT NOT NULL,state TEXT NOT NULL)`,
	} {
		if _, err := tx.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	run := nativeRun("review_evidence_11111111111111111111111111111111")
	if _, err := tx.Exec(`INSERT INTO review_execution_evidence VALUES(?,?)`, run.SourceRunID, f.project); err != nil {
		t.Fatal(err)
	}
	nativeErrorCode(t, transferNativeReadOnly(t, tx, func() error { return transferNativeRunGuard(t.Context(), tx, f.project, nativeSource, 7, run) }), "observation_conflict")
	linked := nativeAttempt("unowned-attempt", "completed")
	linked.SourceRunID = run.SourceRunID
	nativeErrorCode(t, transferNativeReadOnly(t, tx, func() error { return transferNativeAttemptGuard(t.Context(), tx, f.project, nativeSource, 7, linked) }), "observation_conflict")
	for _, state := range []string{"queued", "preparing", "posting", "active", "uncertain", "completed", "held", "failed", "cancelled", "rate_limited"} {
		for _, owner := range []string{"attempt", "operation-pointer"} {
			t.Run(owner+"/"+state, func(t *testing.T) {
				id := "review_attempt_" + owner + state
				var err error
				if owner == "attempt" {
					_, err = tx.Exec(`INSERT INTO review_execution_attempts VALUES(?,?,?)`, id, f.project, state)
				} else {
					_, err = tx.Exec(`INSERT INTO review_operations VALUES(?,?,?,?)`, "operation-"+id, f.project, id, state)
				}
				if err != nil {
					t.Fatal(err)
				}
				attempt := nativeAttempt(id, state)
				nativeErrorCode(t, transferNativeReadOnly(t, tx, func() error { return transferNativeAttemptGuard(t.Context(), tx, f.project, nativeSource, 7, attempt) }), "observation_conflict")
				if err := transferNativeReadOnly(t, tx, func() error {
					return transferNativeAttemptGuard(t.Context(), tx, "other-project", nativeSource, 7, attempt)
				}); err != nil {
					t.Fatal("foreign project blocked display import", err)
				}
				if err := transferNativeReadOnly(t, tx, func() error {
					return transferNativeAttemptGuard(t.Context(), tx, f.project, "other-producer", 7, attempt)
				}); err != nil {
					t.Fatal("separate producer blocked", err)
				}
			})
		}
	}
	for _, scope := range []struct{ project, source string }{{"other-project", nativeSource}, {f.project, "other-producer"}} {
		if err := transferNativeReadOnly(t, tx, func() error { return transferNativeRunGuard(t.Context(), tx, scope.project, scope.source, 7, run) }); err != nil {
			t.Fatal("separate scope blocked", err)
		}
	}
}

func TestTransferNativeMalformedAuthorityFailsClosed(t *testing.T) {
	for _, fixture := range []struct {
		object, ddl string
		run         bool
	}{
		{"evidence-missing-project", `CREATE TABLE review_execution_evidence(id TEXT PRIMARY KEY)`, true},
		{"evidence-view", `CREATE VIEW review_execution_evidence AS SELECT 'x' AS id,'x' AS project_id`, true},
		{"attempt-missing-id", `CREATE TABLE review_execution_attempts(project_id TEXT NOT NULL)`, false},
		{"operation-missing-pointer", `CREATE TABLE review_operations(id TEXT PRIMARY KEY,project_id TEXT NOT NULL)`, false},
	} {
		t.Run(fixture.object, func(t *testing.T) {
			f := newHistoryWriterFixture(t)
			tx := historyWriterTx(t, f)
			if _, err := tx.Exec(fixture.ddl); err != nil {
				t.Fatal(err)
			}
			err := transferNativeReadOnly(t, tx, func() error {
				if fixture.run {
					return transferNativeRunGuard(t.Context(), tx, f.project, nativeSource, 7, nativeRun("display-run"))
				}
				return transferNativeAttemptGuard(t.Context(), tx, f.project, nativeSource, 7, nativeAttempt("display-attempt", "completed"))
			})
			if err == nil {
				t.Fatal(fmt.Sprintf("malformed %s admitted", fixture.object))
			}
		})
	}
}
