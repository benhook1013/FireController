package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/benhook1013/FireController/internal/core"
	"net/http/httptest"
	"testing"
)

func TestStoredUnavailableClassificationPreservesPublicErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		makeError func() error
		kind      storedUnavailableKind
	}{
		{"waiting intent", waitUnavailable, storedWaitingIntent},
		{"allowance", allowanceUnavailable, storedAllowanceObservation},
		{"hosted query", hostedQueryUnavailable, storedHostedQuery},
		{"hosted observation", hostedAllowanceUnavailable, storedHostedObservation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.makeError()
			wrapped := fmt.Errorf("retained evidence: %w", err)
			if !isStoredUnavailable(wrapped, tc.kind) || isStoredUnavailable(wrapped, tc.kind+1) {
				t.Fatal("typed classification lost through wrapping", wrapped)
			}
			var failure *core.Error
			if !errors.As(wrapped, &failure) || failure.Code != "storage_unavailable" || failure.Status != 500 || failure.Message != err.Error() {
				t.Fatal("public error contract changed", failure)
			}
			recorder := httptest.NewRecorder()
			core.WriteError(recorder, wrapped)
			var body struct {
				Error struct{ Code, Message string }
			}
			if recorder.Code != 500 || json.Unmarshal(recorder.Body.Bytes(), &body) != nil || body.Error.Code != failure.Code || body.Error.Message != failure.Message {
				t.Fatal("typed classification escaped the public response", recorder.Code, recorder.Body.String())
			}
			// Human-facing wording is deliberately not classification authority.
			failure.Message = "Reworded retained evidence diagnostic."
			if !isStoredUnavailable(wrapped, tc.kind) {
				t.Fatal("wording edit changed classification")
			}
			lookalike := core.Fail("storage_unavailable", failure.Message, 500)
			if isStoredUnavailable(lookalike, tc.kind) || interruptedAllowanceUnavailable(lookalike) {
				t.Fatal("actual storage error acquired retained-evidence classification")
			}
		})
	}
}

func TestStoredUnavailableDependencyAndRecoveryClassification(t *testing.T) {
	query := fmt.Errorf("lookup: %w", newStoredUnavailable(storedHostedQuery, "Reworded query diagnostic."))
	if !storedAllowanceUnavailable(allowanceDependencyError(query)) {
		t.Fatal("typed missing query did not become unusable allowance")
	}
	storage := core.Fail("storage_unavailable", "Stored hosted allowance query is unavailable.", 500)
	if allowanceDependencyError(storage) != storage || storedAllowanceUnavailable(storage) {
		t.Fatal("storage lookup failure was reclassified by wording")
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, errors.New("database failed")} {
		if allowanceDependencyError(err) != err || interruptedAllowanceUnavailable(err) {
			t.Fatal("real lookup failure was suppressed", err)
		}
	}
	for _, err := range []error{allowanceUnavailable(), newStoredUnavailable(storedHostedObservation, "Reworded hosted observation diagnostic.")} {
		if !interruptedAllowanceUnavailable(fmt.Errorf("recovery: %w", err)) {
			t.Fatal("unusable optional evidence did not allow safe cancellation", err)
		}
	}
	if !isWaitUnavailable(fmt.Errorf("history: %w", newStoredUnavailable(storedWaitingIntent, "Reworded history diagnostic."))) || isWaitUnavailable(core.Fail("storage_unavailable", "Stored review waiting intent is unavailable.", 500)) {
		t.Fatal("history classification still depends on message text")
	}
}

func TestHeldAllowanceOnlyMissingPreflightIsCompatible(t *testing.T) {
	ctx := context.Background()
	f0, a0, _ := cliAllowanceFixture(t)
	if _, err := f0.e.Advance(ctx, attemptProject, a0.ID, a0.Revision, "held", "allowance_exhausted", ""); err != nil {
		t.Fatal(err)
	}
	op0, err := readOperationBase(ctx, f0.store.DB(), attemptProject, a0.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readCLIAllowanceRecord(ctx, f0.store.DB(), op0, a0); !errors.Is(err, errAllowanceObservationAbsent) || !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("absent top-level preflight lost its compatibility sentinel", err)
	}
	if allowance, err := readHeldAllowance(ctx, f0.store.DB(), op0); err != nil || allowance != nil {
		t.Fatal("absent preflight was not treated as legacy no-facts", allowance, err)
	}

	f, a, input := cliAllowanceFixture(t)
	input.Remaining = 0
	if err := f.e.RecordCLIExhaustion(ctx, attemptProject, a.ID, a.Revision, input); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(ctx, attemptProject, a.ID, a.Revision, "held", "allowance_exhausted", ""); err != nil {
		t.Fatal(err)
	}
	op, err := readOperationBase(ctx, f.store.DB(), attemptProject, a.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	deleteRelatedRowWithoutForeignKeys(t, f.store.DB(), "projects", "id", attemptProject)
	if _, err := readHeldAllowance(ctx, f.store.DB(), op); err == nil {
		t.Fatal("missing related project row was hidden as no allowance")
	} else {
		attemptCode(t, err, "storage_unavailable")
	}
	if err := ValidateAllowanceSnapshot(ctx, f.store.DB(), 10); err == nil {
		t.Fatal("snapshot validation accepted missing related project")
	} else {
		attemptCode(t, err, "storage_unavailable")
	}
}

func TestHeldHostedAllowanceMissingQueryIsUnavailable(t *testing.T) {
	ctx := context.Background()
	f, a, input := cooldownObservationFixture(t)
	if err := f.e.RecordHostedCooldown(ctx, attemptProject, a.ID, a.Revision, input); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(ctx, attemptProject, a.ID, a.Revision, "held", "hosted_allowance_cooldown", ""); err != nil {
		t.Fatal(err)
	}
	op, err := readOperationBase(ctx, f.store.DB(), attemptProject, a.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	deleteRelatedRowWithoutForeignKeys(t, f.store.DB(), "review_hosted_allowance_queries", "attempt_id", a.ID)
	if _, err := readHeldAllowance(ctx, f.store.DB(), op); err == nil {
		t.Fatal("missing hosted query was hidden as no allowance")
	} else {
		attemptCode(t, err, "storage_unavailable")
	}
	if err := ValidateAllowanceSnapshot(ctx, f.store.DB(), 10); err == nil {
		t.Fatal("snapshot validation accepted missing hosted query")
	} else {
		attemptCode(t, err, "storage_unavailable")
	}
}

func deleteRelatedRowWithoutForeignKeys(t *testing.T, db *sql.DB, table, column, key string) {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `DELETE FROM `+table+` WHERE `+column+`=?`, key); err != nil {
		t.Fatal("delete related fixture row", table, err)
	}
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
}
