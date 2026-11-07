package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestRunSelectionBusyExposesOnlyOriginalPendingSelection(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, state := range []string{"queued", "selecting"} {
			name := state + "-" + channel
			t.Run(name, func(t *testing.T) {
				f := selectionFixture(t)
				scope := selectionScope(t, f, attemptProject, f.writer, "write")
				var original Run
				var accepted []byte
				receipt, raw := selectionEnqueue(t, f, "accepted-start", channel, nil)
				original, accepted = selectionRead(t, f, receipt.ID), raw
				if state == "selecting" {
					if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
						return updateRun(ctx, tx, original, "selecting", "", "")
					}); err != nil {
						t.Fatal(err)
					}
				}
				before := selectionCounts(t, f)
				// The assertion is deliberately different: a refusal must not claim
				// the other request selected this caller's asserted PR.
				expected := int64(3386)
				for range 2 {
					_, err := f.e.EnqueueRun(t.Context(), scope, "refused-start", RunRequest{Channel: channel, ExpectedPullNumber: &expected})
					var failure *core.Error
					want := map[string]string{"selection_id": original.ID, "channel": channel, "state": state}
					if !errors.As(err, &failure) || failure.Code != "review_busy" || failure.Status != 409 || !reflect.DeepEqual(failure.Details, want) || !strings.Contains(failure.Message, "not accepted") || !strings.Contains(failure.Message, "reviews run-read --selection-id "+original.ID) {
						t.Fatal("busy refusal hid or rebound the original selection", failure)
					}
				}
				if selectionCounts(t, f) != before {
					t.Fatal("refusal created a receipt, selection, operation, attempt or event")
				}
				{
					replayed, err := f.e.EnqueueRun(t.Context(), scope, "accepted-start", RunRequest{Channel: channel})
					if err != nil || !bytes.Equal(replayed, accepted) || selectionCounts(t, f) != before {
						t.Fatal("busy guard intercepted original accepted-key replay", err)
					}
				}
				pending, err := f.e.ReadRun(t.Context(), scope, original.ID)
				if err != nil || pending.State != state || pending.OperationID != "" || pending.WorkerID != original.WorkerID || selectionCounts(t, f) != before {
					t.Fatal("reading returned handle changed the original pending work", pending, err)
				}
				// A pending Hosted selection does not block CLI or another project.
				if _, err := f.e.EnqueueRun(t.Context(), scope, "other-channel", RunRequest{Channel: map[string]string{"cli": "hosted", "hosted": "cli"}[channel]}); err != nil {
					t.Fatal("Hosted selection leaked into CLI admission", err)
				}
				f.configure(t, otherAttemptProject, "test-capability", 0, false)
				other := selectionScope(t, f, otherAttemptProject, f.other, "write")
				raw, err = f.e.EnqueueRun(t.Context(), other, "other-project", RunRequest{Channel: channel})
				if err != nil || bytes.Contains(raw, []byte(original.ID)) {
					t.Fatal("pending selection leaked across projects", string(raw), err)
				}
				if _, err := f.e.ReadRun(t.Context(), other, original.ID); err == nil {
					t.Fatal("foreign project could read returned handle")
				}
			})
		}
	}
}
