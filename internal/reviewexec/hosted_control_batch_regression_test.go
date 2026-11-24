package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestHostedExplicitSelectionSkipsPausedQueuedNeighbor(t *testing.T) {
	f := selectionFixture(t)
	paused := f.enqueue(t, attemptProject, "paused-hosted-neighbor", "hosted", 2, 1)
	schedulingPause(t, f, 2, 0, true)
	independent := int64(4)
	healthy, _ := selectionEnqueue(t, f, "explicit-independent-hosted", "hosted", &independent)
	selectionNext(t, f, &refreshTestReader{})
	if got := selectionRead(t, f, healthy.ID); got.State != "selected" || got.OperationID == "" {
		t.Fatalf("explicit selection did not progress: %+v", got)
	}
	var state string
	if err := f.store.DB().QueryRow(`SELECT state FROM review_operations WHERE id=?`, paused.ID).Scan(&state); err != nil || state != "queued" {
		t.Fatalf("paused operation no longer queued: %q %v", state, err)
	}
}

func TestQueuedNativeSelectionWithUnprovedChildDoesNotStopIndependentSelection(t *testing.T) {
	f := selectionFixture(t)
	broken, _ := selectionEnqueue(t, f, "linked-native-selection", "hosted", nil)
	healthy, _ := selectionEnqueue(t, f, "healthy-linked-native-neighbor", "cli", nil)
	child := f.enqueue(t, attemptProject, "retained-native-hosted-child", "hosted", 4, 1)
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `PRAGMA ignore_check_constraints=ON`); err != nil {
			return err
		}
		_, corruptErr := tx.ExecContext(ctx, `UPDATE review_selections
			SET operation_id=?,created_at='2000-01-01T00:00:00Z' WHERE id=?`, child.ID, broken.ID)
		_, resetErr := tx.ExecContext(ctx, `PRAGMA ignore_check_constraints=OFF`)
		return errors.Join(corruptErr, resetErr)
	})
	if err != nil {
		t.Fatal(err)
	}
	selectionNext(t, f, &refreshTestReader{})
	if got := selectionRead(t, f, healthy.ID); got.State != "selected" || got.OperationID == "" {
		t.Fatalf("independent selection did not progress: %+v", got)
	}
	var state, linkedID string
	if err := f.store.DB().QueryRowContext(t.Context(), `SELECT state,operation_id FROM review_selections WHERE id=?`, broken.ID).Scan(&state, &linkedID); err != nil || state != "queued" || linkedID != child.ID {
		t.Fatalf("unproved custody was rewritten: %q %q %v", state, linkedID, err)
	}
}

func TestSourceRecoveryRefusesUnknownEventlessAcceptanceOrder(t *testing.T) {
	for _, tc := range []struct {
		name, createdAt string
		keepEvent, want bool
	}{
		{"eventless-earlier", "2001-01-01T00:00:00Z", false, false},
		{"eventless-unknown", "unknown", false, true},
		{"later-event-authoritative", "2001-01-01T00:00:00Z", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := selectionFixture(t)
			base, _ := selectionEnqueue(t, f, "recovery-target", "hosted", nil)
			neighbor := f.enqueue(t, attemptProject, "retained-intent", "hosted", 2, 1)
			run := selectionRead(t, f, base.ID)
			admissionSQL(t, f, `UPDATE review_operations SET created_at=? WHERE id=?`, tc.createdAt, neighbor.ID)
			if !tc.keepEvent {
				admissionSQL(t, f, `DELETE FROM events WHERE event_type='review.requested' AND resource_id=?`, neighbor.ID)
			}
			err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				got, err := automaticSourceSuperseded(ctx, tx, run)
				if err != nil || got != tc.want {
					t.Fatalf("superseded: got %v want %v: %v", got, tc.want, err)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNativeCustodyRejectsConflictingLiveSelectionLink(t *testing.T) {
	f := selectionFixture(t)
	original, _ := selectionEnqueue(t, f, "original-with-child", "hosted", nil)
	selectionNext(t, f, &refreshTestReader{})
	child := selectionRead(t, f, original.ID).OperationID
	if child == "" {
		t.Fatal("missing child")
	}
	admissionSQL(t, f, `UPDATE review_selections SET state='held',reason='linked_selection_quarantined',operation_id=NULL,revision=3 WHERE id=?`, original.ID)
	admissionSQL(t, f, `INSERT INTO review_selection_quarantine (selection_id,project_id,original_operation_id,original_state,observed_revision) VALUES(?,?,?,'selecting',2)`, original.ID, attemptProject, child)
	if err := requireUnquarantinedSelectionLink(t.Context(), f.store.DB(), attemptProject, child); !isLinkedSelectionCustodyHeld(err) {
		t.Fatal("expected proved hold", err)
	}
	live, _ := selectionEnqueue(t, f, "different-live-selector", "hosted", nil)
	admissionSQL(t, f, `UPDATE review_selections SET state='selected',operation_id=? WHERE id=?`, child, live.ID)
	if err := requireUnquarantinedSelectionLink(t.Context(), f.store.DB(), attemptProject, child); err == nil || isLinkedSelectionCustodyHeld(err) {
		t.Fatal("conflicting live attribution was considered proved", err)
	}
}

func TestSelectionSnapshotDistinguishesLegacyEventFromMissingPinnedEvent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pin     *int64
		missing bool
	}{
		{"legacy-real", nil, false},
		{"pin-missing", new(int64), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := selectionFixture(t)
			if tc.pin != nil {
				*tc.pin = 2
			}
			r, _ := selectionEnqueue(t, f, "requested-event-control", "hosted", tc.pin)
			if tc.missing {
				admissionSQL(t, f, `DELETE FROM events WHERE event_type='review.selection_requested' AND resource_id=?`, r.ID)
			} else {
				admissionSQL(t, f, `UPDATE events SET payload_json='{"channel":"hosted"}' WHERE event_type='review.selection_requested' AND resource_id=?`, r.ID)
			}
			err := ValidateSelectionSnapshot(t.Context(), f.store.DB())
			if (err != nil) != tc.missing {
				t.Fatalf("validate snapshot missing=%t: %v", tc.missing, err)
			}
		})
	}
}

func TestContinuityValidatesCurrentBasisAndNativeProjectActor(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		actor         any
		valid         bool
	}{
		{"legacy-hosted", `{"automatic_hosted":true}`, "test-actor", true},
		{"invalid-basis", `{"automatic_hosted":true,"hosted_basis_revision":"bad","cli_basis_revision":1}`, "test-actor", false},
		{"same-value-blob", `{"automatic_hosted":true}`, []byte("test-actor"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := schedulingEventSnapshotFixture(t)
			_, err := tx.ExecContext(t.Context(), `INSERT INTO events(project_id,resource_id,event_type,revision,actor_id,payload_json) VALUES
			 ('test-project','test-project','review.scheduling_updated',1,'test-actor','{"automatic_hosted":true}'),
			 ('test-project','test-project','review.scheduling_updated',2,?,?)`, tc.actor, tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			current := schedulingRecord{Scheduling: Scheduling{ProjectID: "test-project", Revision: 2, AutomaticHosted: true}, actor: "test-actor"}
			ok, err := automaticChannelContinuous(t.Context(), tx, Run{ProjectID: "test-project", Channel: "hosted"}, 1, "test-actor", current)
			if ok != tc.valid || err != nil {
				t.Fatalf("continuity: got %v %v; expected valid=%v", ok, err, tc.valid)
			}
		})
	}
}
