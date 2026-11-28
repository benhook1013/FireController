package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

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
