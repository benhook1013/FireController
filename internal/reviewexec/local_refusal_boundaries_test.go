package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestDraftRereadRefusalOnlySkipsItsOwnCandidate(t *testing.T) {
	for _, tt := range []struct {
		name       string
		refusal    error
		wantChecks int
	}{
		{"retained-decode", &retainedOperationDecodeError{errors.New("synthetic typed decoder refusal")}, 2},
		{"shared-storage", errors.New("synthetic unclassified SQL failure"), 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{}
			checks, releases := 0, 0
			batch := []Operation{{ProjectID: "project-first", ID: "op-first", Channel: "cli"}, {ProjectID: "project-next", ID: "op-next", Channel: "cli"}}
			check := func(_ context.Context, got Operation, _ map[string]Driver) (bool, bool, bool, error) {
				if got.ID != batch[checks].ID || got.ProjectID != batch[checks].ProjectID {
					t.Fatalf("candidate %d changed identity: %+v", checks, got)
				}
				checks++
				if checks == 1 {
					return false, false, false, tt.refusal
				}
				// Prove the next project's draft check runs; it remains waiting.
				return false, false, false, nil
			}
			err := e.processPendingBatchWithDraftCheck(t.Context(), nil, func() { releases++ }, 0, batch, pendingExhaustion{}, check)
			if tt.wantChecks == 1 {
				attemptCode(t, err, "review_persistence_failed")
			} else if err != nil {
				t.Fatal(err)
			}
			if checks != tt.wantChecks || releases != checks {
				t.Fatalf("checks=%d releases=%d want=%d", checks, releases, tt.wantChecks)
			}
		})
	}
}

func TestAutomaticAdoptionCountRetainsLocalRepositoryRefusal(t *testing.T) {
	f := selectionFixture(t)
	retainObligationSource(t, f, attemptProject, "local-refusal-importer", "local-refusal-source", strings.Repeat("c", 64), 0)
	if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if count, err := sourceObligationCount(ctx, tx, attemptProject, 2); count != 1 || err != nil {
			t.Fatalf("valid retained source count=%d err=%v", count, err)
		}
		repositoryChanged := core.Fail("repository_changed", "Synthetic retained source bound to a different repository.", 409)
		for _, tt := range []struct {
			failure error
			local   bool
		}{
			{repositoryChanged, true},
			{errors.New("synthetic shared SQL failure"), false},
			{context.Canceled, false},
			{context.DeadlineExceeded, false},
		} {
			count, err := sourceObligationCount(ctx, admissionObligationFailure{Tx: tx, failure: tt.failure}, attemptProject, 2)
			if count != 0 || err == nil || automaticProjectRefused(err) != tt.local || isSourceObligationProjectStateError(err) {
				t.Errorf("automatic read count=%d local=%t ordinary-admission marker=%t err=%v", count, automaticProjectRefused(err), isSourceObligationProjectStateError(err), err)
			}
			if tt.local {
				attemptCode(t, err, "review_held")
				attemptCode(t, sourceObligationReadError(tt.failure), "review_held")
				if preparationReason(err) != "repository_changed" || automaticProjectRefused(sourceObligationReadError(tt.failure)) {
					t.Errorf("public reason/ordinary admission changed: %v", err)
				}
			} else if !errors.Is(err, tt.failure) {
				t.Errorf("shared error lost: got %v want %v", err, tt.failure)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPreferredSelectionKeysRequireNativeText(t *testing.T) {
	f := selectionFixture(t)
	// Synthetic row expression only. No malformed state is written to a store.
	rows, err := f.store.DB().QueryContext(t.Context(), `SELECT 'first', CAST('one' AS BLOB) UNION ALL SELECT 'next', 'one' UNION ALL SELECT 'last', 'two'`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readPreferredSelectionIDs(rows)
	want := []pendingCandidateID{{project: "next", operation: "one"}, {project: "last", operation: "two"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("preferred keys %+v, err=%v; want %+v", got, err, want)
	}

	rows, err = f.store.DB().QueryContext(t.Context(), `SELECT 'first', 'one' UNION ALL SELECT 'next', abs(-9223372036854775808)`)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := readPreferredSelectionIDs(rows); got != nil || err == nil {
		t.Fatalf("real SQLite iteration failure became success: keys=%+v err=%v", got, err)
	}
}
