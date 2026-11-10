package reviewexec

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestWaitingReviewSurvivesOnlyQueueChangesAfterItsTarget(t *testing.T) {
	for _, scenario := range []string{"remove later", "append later", "replace later", "move predecessor", "remove target", "mode", "channels", "capability", "paused then restored", "pause only then restored", "explicit unchanged replacement"} {
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
			case "pause only then restored":
				c.Paused = true
				change("pause")
				c.Paused = false
				safe = true
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

func TestWaitingReviewChannelSetCompatibilityAndUnchangedQueueBarrier(t *testing.T) {
	for _, test := range []struct {
		name            string
		initialChannels []string
		nextChannels    []string
		nextQueue       []int64
		pauseResume     bool
		compatible      bool
	}{
		{name: "reordered channels with suffix edit", initialChannels: []string{"cli", "hosted"}, nextChannels: []string{"hosted", "cli"}, nextQueue: []int64{2, 6}, compatible: true},
		{name: "reordered channels alone keeps unchanged queue barrier", initialChannels: []string{"cli", "hosted"}, nextChannels: []string{"hosted", "cli"}, nextQueue: []int64{2, 4}},
		{name: "channel added", initialChannels: []string{"cli"}, nextChannels: []string{"cli", "hosted"}, nextQueue: []int64{2, 6}},
		{name: "channel removed", initialChannels: []string{"cli", "hosted"}, nextChannels: []string{"cli"}, nextQueue: []int64{2, 6}},
		{name: "pause resume with reorder and suffix edit", initialChannels: []string{"cli", "hosted"}, nextChannels: []string{"hosted", "cli"}, nextQueue: []int64{2, 6}, pauseResume: true, compatible: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAttemptFixture(t)
			configure := func(key string, expected int64, c Configuration) int64 {
				t.Helper()
				c.ExpectedRevision = &expected
				raw, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), key, c)
				if err != nil {
					t.Fatal(err)
				}
				var receipt Receipt
				if json.Unmarshal(raw, &receipt) != nil {
					t.Fatal("configuration receipt")
				}
				return receipt.Revision
			}
			c := Configuration{CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4}, Channels: append([]string(nil), test.initialChannels...)}
			revision := configure("initial", 0, c)
			request := f.enqueue(t, attemptProject, "queued", "cli", 2, revision)
			if test.pauseResume {
				c.Paused = true
				revision = configure("pause", revision, c)
				c.Paused = false
				c.Channels = append([]string(nil), test.nextChannels...)
				revision = configure("resume-reordered", revision, c)
				c.Queue = append([]int64(nil), test.nextQueue...)
			} else {
				c.Channels = append([]string(nil), test.nextChannels...)
				c.Queue = append([]int64(nil), test.nextQueue...)
			}
			configure("configuration-change", revision, c)

			attempt, err := f.e.StartAttempt(t.Context(), attemptProject, request.ID)
			if err != nil {
				t.Fatal(err)
			}
			operation, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), request.ID)
			if err != nil {
				t.Fatal(err)
			}
			if test.compatible {
				if attempt.ID == "" || attempt.State != "preparing" || operation.State != "preparing" {
					t.Fatal("compatible channel-set change held original request", attempt, operation)
				}
			} else if attempt.ID != "" || operation.State != "held" || operation.Reason != "configuration_changed" {
				t.Fatal("incompatible channel or unchanged-queue edit admitted stale request", attempt, operation)
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

func TestSourceConfigurationChannelSetsPreserveApplicability(t *testing.T) {
	for _, test := range []struct {
		name          string
		initial, next []string
		valid         bool
	}{
		{name: "reordered", initial: []string{"cli", "hosted"}, next: []string{"hosted", "cli"}, valid: true},
		{name: "added", initial: []string{"cli"}, next: []string{"cli", "hosted"}},
		{name: "removed", initial: []string{"cli", "hosted"}, next: []string{"cli"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := Configuration{CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2, 4}, Channels: test.initial}
			current := original
			current.Channels = test.next
			beforeOriginal, beforeCurrent := encoded(original), encoded(current)
			if got := sameSourceConfiguration(current, original); got != test.valid {
				t.Fatal("whole source channel-set applicability", got, test.valid)
			}
			if got := sameSourceTargetConfiguration(current, original, 2, 4); got != test.valid {
				t.Fatal("target source channel-set applicability", got, test.valid)
			}
			if encoded(original) != beforeOriginal || encoded(current) != beforeCurrent {
				t.Fatal("source comparison mutated a channel list")
			}
			current.Queue = []int64{2, 4, 6}
			current.Paused = true
			beforeCurrent = encoded(current)
			if got := sameSourceTargetConfiguration(current, original, 2, 4); got != test.valid {
				t.Fatal("suffix and pause source applicability", got, test.valid)
			}
			if sameSourceConfiguration(current, original) {
				t.Fatal("whole-queue pause guard was relaxed")
			}
			if encoded(original) != beforeOriginal || encoded(current) != beforeCurrent {
				t.Fatal("source comparison mutated retained plans")
			}
		})
	}
}

func TestWaitingReviewRejectsDamagedConfigurationHistory(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, damage := range []string{"revision gap", "malformed JSON", "oversized JSON", "unknown field", "duplicate field", "invalid shape"} {
			t.Run(channel+"/"+damage, func(t *testing.T) {
				f := supervisorFixture(t)
				request := f.enqueue(t, attemptProject, "queued-original", channel, 2, 1)
				for revision := int64(1); revision <= 2; revision++ {
					c := Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4, 6}, Channels: []string{"cli", "hosted"}}
					if revision == 2 {
						c.Queue = append(c.Queue, 8)
					}
					if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), fmt.Sprintf("suffix-%d", revision), c); err != nil {
						t.Fatal(err)
					}
				}
				var body string
				if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_config WHERE project_id=? AND revision=2`, attemptProject).Scan(&body); err != nil {
					t.Fatal(err)
				}
				switch damage {
				case "revision gap":
					admissionSQL(t, f, `DELETE FROM review_execution_config WHERE project_id=? AND revision=2`, attemptProject)
				case "malformed JSON":
					body = `{`
				case "oversized JSON":
					body += strings.Repeat(" ", 8192)
				case "unknown field":
					body = strings.TrimSuffix(body, "}") + `,"unknown":true}`
				case "duplicate field":
					body = strings.TrimSuffix(body, "}") + `,"paused":false}`
				case "invalid shape":
					body = strings.Replace(body, `"expected_revision":1`, `"expected_revision":null`, 1)
				}
				if damage != "revision gap" {
					admissionSQL(t, f, `UPDATE review_execution_config SET body_json=? WHERE project_id=? AND revision=2`, body, attemptProject)
				}
				attempt, err := f.e.StartAttempt(t.Context(), attemptProject, request.ID)
				attemptCode(t, err, "storage_unavailable")
				if attempt.ID != "" {
					t.Fatal("damaged history created an attempt", attempt)
				}
				driver := &controlledDriver{}
				attemptCode(t, f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}), "review_persistence_failed")
				op, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), request.ID)
				if err != nil || op.State != "queued" || op.Reason != "" || op.Revision != request.Revision || op.ConfigurationRevision != 1 || op.AttemptID != "" {
					t.Fatal("storage damage became a normal configuration hold or changed original request", op, err)
				}
				if driver.prepares.Load() != 0 || driver.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
					t.Fatal("damaged history acquired capacity or invoked the provider")
				}
			})
		}
	}
}

func TestQueuedConfigurationRejectsTruncatedHistory(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	request := f.enqueue(t, attemptProject, "queued-original", "cli", 2, 1)
	revision := int64(1)
	c := Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4, 6}, Channels: []string{"cli", "hosted"}}
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "suffix", c); err != nil {
		t.Fatal(err)
	}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	o, err := readOperation(t.Context(), tx, attemptProject, request.ID)
	if err != nil {
		t.Fatal(err)
	}
	current, revision, err := readConfiguration(t.Context(), tx, attemptProject)
	if err != nil {
		t.Fatal(err)
	}
	// The caller's retained upper revision must be present, even when no later
	// row exists to reveal a gap inside the loop.
	if _, err := tx.ExecContext(t.Context(), `DELETE FROM review_execution_config WHERE project_id=? AND revision=?`, attemptProject, revision); err != nil {
		t.Fatal(err)
	}
	compatible, err := compatibleQueuedConfiguration(t.Context(), tx, o, current, revision)
	attemptCode(t, err, "storage_unavailable")
	if compatible {
		t.Fatal("truncated history was compatible")
	}
}
