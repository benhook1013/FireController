package reviewexec

import (
	"context"
	"encoding/json"
	"testing"
)

func TestWaitingReviewSurvivesOnlyQueueChangesAfterItsTarget(t *testing.T) {
	for _, scenario := range []string{"remove later", "append later", "replace later", "move predecessor", "remove target", "mode", "channels", "capability", "paused then restored", "explicit unchanged replacement"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			request := f.enqueue(t, attemptProject, "original-request", "cli", 2, 1)
			c := Configuration{CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}}
			revision := int64(1)
			change := func(key string) {
				t.Helper()
				c.ExpectedRevision = &revision
				raw, err := f.e.Configure(context.Background(), f.scope(t, attemptProject, f.admin, true), key, c)
				if err != nil {
					t.Fatal(err)
				}
				var r Receipt
				if json.Unmarshal(raw, &r) != nil {
					t.Fatal("receipt")
				}
				revision = r.Revision
			}
			safe := false
			switch scenario {
			case "remove later":
				c.Queue = []int64{2}
				safe = true
			case "append later":
				c.Queue = []int64{2, 4, 6}
				safe = true
			case "replace later":
				c.Queue = []int64{2, 6}
				safe = true
			case "move predecessor":
				c.Queue = []int64{4, 2}
			case "remove target":
				c.Queue = []int64{4}
			case "mode":
				c.Mode = "stacked"
			case "channels":
				c.Channels = []string{"cli"}
			case "capability":
				c.CapabilityID = "other-account"
			case "paused then restored":
				c.Paused = true
				change("pause")
				c.Paused = false
				c.Queue = []int64{2, 6}
			}
			change("queue-change")
			attempt, err := f.e.StartAttempt(context.Background(), attemptProject, request.ID)
			if err != nil {
				t.Fatal(err)
			}
			operation, err := f.e.ReadOperation(context.Background(), f.scope(t, attemptProject, f.writer, false), request.ID)
			if err != nil {
				t.Fatal(err)
			}
			if operation.ConfigurationRevision != 1 {
				t.Fatal("original configuration provenance changed")
			}
			if safe {
				if attempt.ID == "" || attempt.State != "preparing" {
					t.Fatal("harmless later queue edit held original request", operation)
				}
			} else if attempt.ID != "" || operation.State != "held" || operation.Reason != "configuration_changed" {
				t.Fatal("meaningful configuration change admitted stale request", operation)
			}
		})
	}
}

func TestPreparingReviewRechecksEveryInterveningQueueRevision(t *testing.T) {
	for _, meaningful := range []bool{false, true} {
		t.Run(map[bool]string{false: "suffix only", true: "prefix moved then restored"}[meaningful], func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			r := f.enqueue(t, attemptProject, "preparing-request", "cli", 2, 1)
			a, err := f.e.StartAttempt(context.Background(), attemptProject, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			revision := int64(1)
			c := Configuration{CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 6}, Channels: []string{"cli", "hosted"}}
			if meaningful {
				c.Queue = []int64{4, 2}
			}
			for i := 0; i < 2; i++ {
				c.ExpectedRevision = &revision
				raw, err := f.e.Configure(context.Background(), f.scope(t, attemptProject, f.admin, true), []string{"first-edit", "second-edit"}[i], c)
				if err != nil {
					t.Fatal(err)
				}
				var receipt Receipt
				if json.Unmarshal(raw, &receipt) != nil {
					t.Fatal("receipt")
				}
				revision = receipt.Revision
				c.Queue = []int64{2, 8}
			}
			admitted, err := f.e.Admit(context.Background(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
			if meaningful {
				if err == nil {
					t.Fatal("restored prefix hid earlier change")
				}
			} else if err != nil || admitted.State != "active" {
				t.Fatal("suffix edits invalidated prepared original comparison", admitted, err)
			}
		})
	}
}

func TestSupervisorKeepsOriginalRequestAcrossLaterQueueCleanup(t *testing.T) {
	f := supervisorFixture(t)
	r := f.enqueue(t, attemptProject, "waiting-original", "cli", 2, 1)
	revision := int64(1)
	_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "unrelated-cleanup", Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2}, Channels: []string{"cli", "hosted"}})
	if err != nil {
		t.Fatal(err)
	}
	driver := &controlledDriver{prepare: func(ctx context.Context, o Operation, c Configuration, a Attempt) (Preparation, error) {
		if o.ID != r.ID || o.ConfigurationRevision != 1 || len(c.Queue) != 2 || c.Queue[1] != 4 {
			t.Fatal("preparation lost original configuration")
		}
		revision := int64(2)
		_, err := f.e.Configure(ctx, f.scope(t, attemptProject, f.admin, true), "later-add-during-preparation", Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 6}, Channels: []string{"cli", "hosted"}})
		if err != nil {
			return Preparation{}, err
		}
		return Preparation{Anchor: attemptAnchor(attemptProject, 2)}, nil
	}}
	if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
		t.Fatal(err)
	}
	operation, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), r.ID)
	if err != nil || operation.State != "completed" || operation.ConfigurationRevision != 1 || driver.prepares.Load() != 1 || driver.calls.Load() != 1 {
		t.Fatal("unrelated cleanup disrupted or duplicated original execution", operation, err)
	}
	if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
		t.Fatal(err)
	}
	if driver.calls.Load() != 1 {
		t.Fatal("completed original request dispatched again")
	}
}
