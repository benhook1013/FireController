package reviewexec

import (
	"crypto/sha256"
	"fmt"
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
	priorityPending(t, f)
	if _, err := f.e.Advance(t.Context(), otherAttemptProject, busy.ID, 1, "cancelled", "synthetic_release", ""); err != nil {
		t.Fatal(err)
	}
	priorityPending(t, f, head.ID)
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
	lower := explicitOrderEnqueue(t, f, "lower-bypass", "hosted", true, 1)
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
