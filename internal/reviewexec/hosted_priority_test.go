package reviewexec

import (
	"bytes"
	"context"

	"log/slog"
	"strings"

	"crypto/sha256"
	"fmt"
	"github.com/benhook1013/FireController/internal/core"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func priorityCooldown(t *testing.T, f *attemptFixture, request Receipt, number int64, delay time.Duration) time.Time {
	t.Helper()
	a := f.start(t, attemptProject, request)
	anchor := attemptAnchor(attemptProject, number)
	q, _, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, reviewprovider.Author{ID: 12, Login: "poster", Kind: "User"}, reviewprovider.Author{ID: 30, Login: "coderabbitai[bot]", Kind: "Bot"})
	if err != nil {
		t.Fatal(err)
	}
	q, err = f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, hostedQueryComment(t, q))
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(a.ID)))
	observed := time.Now().UTC()
	response := q.Comment.CreatedAt.Add(time.Nanosecond)
	due := response.Add(delay)
	in := HostedAllowanceInput{QueryID: q.ID, Anchor: anchor, ObservedAt: observed.Format(time.RFC3339Nano), ChangedFiles: 2, CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1234, Observation: reviewprovider.HostedAllowanceObservation{State: "cooldown_observed", Reason: "provider_cooldown_observed", QueryCommentID: q.Comment.ID, ResponseCommentID: 92, QueryCreatedAt: q.Comment.CreatedAt, ResponseCreatedAt: response, RetryNotBefore: due}}
	if _, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, HostedObservationInput{Kind: "allowance_query", CaptureID: in.CaptureID, CaptureSHA256: digest, CaptureBytes: in.CaptureBytes, StartedAt: q.Comment.CreatedAt.Format(time.RFC3339Nano), FinishedAt: in.ObservedAt, ObservedAt: in.ObservedAt, ExpectedAnchor: anchor, Intent: q.Intent}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.ScheduleHostedCooldown(t.Context(), attemptProject, a.ID, 1); err != nil {
		t.Fatal(err)
	}
	return due
}

func priorityPending(t *testing.T, f *attemptFixture, want ...string) {
	t.Helper()
	got, err := f.e.pending(t.Context())
	if err != nil || len(got) != len(want) {
		t.Fatalf("pending count=%d want=%d: %v %+v", len(got), len(want), err, got)
	}
	ids := map[string]bool{}
	for _, o := range got {
		ids[o.ID] = true
	}
	for _, id := range want {
		if !ids[id] {
			t.Fatalf("missing pending %s: %+v", id, got)
		}
	}
}

func TestHostedPriorityCooldownAndAtomicClaims(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	// The farther request was created first and its genuine retained cooldown
	// expires first. Neither fact permits it to pass the nearer queued intent.
	lower := f.enqueue(t, attemptProject, "lower-first", "hosted", 4, 1)
	lowerDue := priorityCooldown(t, f, lower, 4, 3*time.Minute)
	head := f.enqueue(t, attemptProject, "head-second", "hosted", 2, 1)
	headDue := priorityCooldown(t, f, head, 2, 3*time.Minute+5*time.Second)
	now := lowerDue
	f.e.schedulingNow = func() time.Time { return now }
	priorityPending(t, f)
	if a, err := f.e.StartAttempt(t.Context(), attemptProject, lower.ID); err != nil || a.ID != "" {
		t.Fatal("lower passed future head", a, err)
	}
	if got := attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`); got != 0 {
		t.Fatal("priority reserved provider capacity", got)
	}
	busy := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "busy", "hosted", 2, 1))
	now = headDue
	// Another repository's Hosted reservation does not consume this repo's
	// slot; the nearer queued intent still excludes the lower request.
	priorityPending(t, f)
	if _, err := f.e.Advance(t.Context(), otherAttemptProject, busy.ID, 1, "cancelled", "synthetic_release", ""); err != nil {
		t.Fatal(err)
	}
	priorityPending(t, f)
	// Race two independently constructed workers through the real serialized
	// claim. Lower is refused whether it runs before or after the head claim.
	other, err := New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	other.schedulingNow = f.e.schedulingNow
	var wg sync.WaitGroup
	results := make(chan Attempt, 2)
	errors := make(chan error, 2)
	start := make(chan struct{})
	for _, claim := range []struct {
		e  *Engine
		id string
	}{{f.e, lower.ID}, {other, head.ID}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			a, err := claim.e.StartAttempt(t.Context(), attemptProject, claim.id)
			results <- a
			errors <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			attemptCode(t, err, "review_busy")
		}
	}
	claimed := 0
	for a := range results {
		if a.ID != "" {
			claimed++
			if a.OperationID != head.ID {
				t.Fatal("lower won concurrent claim", a)
			}
		}
	}
	if claimed != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
		t.Fatal("claim/accounting changed", claimed)
	}
}

func TestHostedPriorityOnlyUnpausedProjectHostedIntents(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	lower := f.enqueue(t, attemptProject, "lower-bypass", "hosted", 4, 1)
	// Preserve the migrated flag's semantics without requiring the later
	// public explicit-order request option in this prerequisite slice.
	admissionSQL(t, f, `UPDATE review_operations SET allow_out_of_order=1 WHERE id=?`, lower.ID)
	cli := f.enqueue(t, attemptProject, "head-cli", "cli", 2, 1)
	other := f.enqueue(t, otherAttemptProject, "other-head", "hosted", 2, 1)
	// Front queue membership alone and a different channel/project are not
	// Hosted intent in this project. The lower request remains selectable.
	priorityPending(t, f, lower.ID, cli.ID, other.ID)
	head := f.enqueue(t, attemptProject, "head-hosted", "hosted", 2, 1)
	priorityPending(t, f, head.ID, cli.ID, other.ID)
	if a, err := f.e.StartAttempt(t.Context(), attemptProject, lower.ID); err != nil || a.ID != "" {
		t.Fatal("dependency bypass also bypassed priority", a, err)
	}
	timerControl(t, f, head.ID, "pause", "pause-head")
	priorityPending(t, f, lower.ID, cli.ID, other.ID)
	timerControl(t, f, head.ID, "resume", "resume-head")
	priorityPending(t, f, head.ID, cli.ID, other.ID)
	f.configure(t, attemptProject, "test-capability", 1, true)
	priorityPending(t, f, other.ID)
	if a, err := f.e.StartAttempt(t.Context(), attemptProject, lower.ID); err != nil || a.ID != "" {
		t.Fatal("project pause bypassed", a, err)
	}
	f.configure(t, attemptProject, "test-capability", 2, false)
	timerControl(t, f, head.ID, "cancel", "cancel-head")
	priorityPending(t, f, lower.ID, cli.ID, other.ID)
	f.start(t, attemptProject, lower)
}

func TestHostedPriorityInvalidIntentCannotReserveTurn(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	head := f.enqueue(t, attemptProject, "invalid-head", "hosted", 2, 1)
	lower := f.enqueue(t, attemptProject, "valid-lower", "hosted", 4, 1)
	// The head no longer has its original capability binding. It must remain
	// visible for its normal hold, without permanently reserving a queue turn.
	admissionSQL(t, f, `UPDATE review_operations SET capability_fingerprint='retired-binding' WHERE id=?`, head.ID)
	priorityPending(t, f, head.ID, lower.ID)
	if a, err := f.e.StartAttempt(t.Context(), attemptProject, head.ID); err != nil || a.ID != "" {
		t.Fatal(a, err)
	}
	op, err := readOperation(t.Context(), f.store.DB(), attemptProject, head.ID)
	if err != nil || op.State != "held" || op.Reason != "capability_changed" {
		t.Fatal("invalid intent not resolved normally", op, err)
	}
	f.start(t, attemptProject, lower)
}

// Damaged retained history cannot authorize a queue jump or stop another project.
func TestPriorityConfigurationHistoryIsolation(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, damaged := range []string{"candidate", "predecessor"} {
			for _, damage := range []string{"malformed", "gap"} {
				t.Run(channel+"/"+damaged+"/"+damage, func(t *testing.T) {
					f := supervisorFixture(t)
					f.configure(t, otherAttemptProject, "other-account", 0, false)
					number := int64(4)
					if damaged == "predecessor" {
						number = 2
					}
					bad := f.enqueue(t, attemptProject, "damaged-history", channel, number, 1)
					for revision := int64(1); revision <= 2; revision++ {
						c := Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4, 6}, Channels: []string{"cli", "hosted"}}
						if revision == 2 {
							c.Queue = append(c.Queue, 8)
						}
						if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), fmt.Sprintf("suffix-%d", revision), c); err != nil {
							t.Fatal(err)
						}
					}
					otherNumber := int64(2)
					if damaged == "predecessor" {
						otherNumber = 4
					}
					same := f.enqueue(t, attemptProject, "same-project", channel, otherNumber, 3)
					healthy := f.enqueue(t, otherAttemptProject, "healthy-project", channel, 2, 1)
					if damage == "gap" {
						admissionSQL(t, f, `DELETE FROM review_execution_config WHERE project_id=? AND revision=2`, attemptProject)
					} else {
						admissionSQL(t, f, `UPDATE review_execution_config SET body_json='private-corrupt-history' WHERE project_id=? AND revision=2`, attemptProject)
					}
					before, err := readOperation(t.Context(), f.store.DB(), attemptProject, bad.ID)
					if err != nil {
						t.Fatal(err)
					}
					var output bytes.Buffer
					previous := slog.Default()
					slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
					defer slog.SetDefault(previous)
					seen := map[string]int{}
					driver := &controlledDriver{prepare: func(_ context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
						seen[o.ID]++
						return Preparation{}, core.Fail("review_evidence_pending", "synthetic hold", 409)
					}}
					for range 3 {
						if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver, "other-account": driver}); err != nil {
							t.Fatal("candidate history stopped shared scheduling", err)
						}
					}
					after, err := readOperation(t.Context(), f.store.DB(), attemptProject, bad.ID)
					if err != nil || encoded(after) != encoded(before) {
						t.Fatal("damaged request mutated", before, after, err)
					}
					if seen[bad.ID] != 0 || seen[healthy.ID] != 1 || driver.calls.Load() != 0 {
						t.Fatal("provider work or healthy-project isolation", seen, driver.calls.Load())
					}
					if damaged == "predecessor" && seen[same.ID] != 0 {
						t.Fatal("unproven predecessor allowed queue jump", seen)
					}
					if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
						t.Fatal("history failure retained capacity")
					}
					if !strings.Contains(output.String(), bad.ID) || strings.Contains(output.String(), "private-corrupt-history") {
						t.Fatal("missing or unsafe candidate diagnostic", output.String())
					}
					if got := strings.Count(output.String(), `"operation_id":"`+bad.ID+`"`); got != 1 {
						t.Fatal("repeated diagnostic was not bounded", got, output.String())
					}
				})
			}
		}
	}
}
