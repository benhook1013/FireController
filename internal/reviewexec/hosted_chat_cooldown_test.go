package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func assertChatHold(t *testing.T, err error) {
	t.Helper()
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "review_held" {
		t.Fatalf("expected chat hold, got %v", err)
	}
	details, _ := failure.Details.(map[string]string)
	if details["reason"] != "hosted_chat_cooldown" {
		t.Fatal(details)
	}
}

func TestHostedChatCooldownSharedGateRestartExpiryAndScope(t *testing.T) {
	f, a, in := cooldownObservationFixture(t)
	in.Observation.Reason = reviewprovider.HostedChatCooldownReason
	if err := f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal(err)
	}
	q, err := f.e.ReadHostedAllowanceQuery(t.Context(), attemptProject, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", "hosted_chat_cooldown", ""); err != nil {
		t.Fatal(err)
	}
	w := statusHTTP(t, f, a.OperationID, "", "")
	var compact OperationStatus
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &compact) != nil || compact.RetryNotBefore != in.Observation.RetryNotBefore.Format(time.RFC3339Nano) {
		t.Fatal("chat compact deadline", w.Code, w.Body.String())
	}
	// Reopening reconstructs the negative gate from retained facts only.
	cliAllowanceReopen(t, f)
	if err := ValidateAllowanceSnapshot(t.Context(), f.store.DB(), 10); err != nil {
		t.Fatal("backup validation", err)
	}
	check := func(account string, poster, bot reviewprovider.Author, now time.Time) error {
		return f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			return checkHostedChatCooldown(ctx, tx, a.Provider, account, poster, bot, now)
		})
	}
	assertChatHold(t, check(a.Account, q.PostingActor, q.ProviderActor, time.Now().UTC()))
	assertChatHold(t, check(a.Account, q.PostingActor, q.ProviderActor, in.Observation.RetryNotBefore.Add(-time.Nanosecond)))
	for _, scope := range []string{"account", "poster", "provider", "expired"} {
		account, poster, bot, now := a.Account, q.PostingActor, q.ProviderActor, time.Now().UTC()
		switch scope {
		case "account":
			account = "different-account"
		case "poster":
			poster.ID++
		case "provider":
			bot.ID++
		case "expired":
			now = in.Observation.RetryNotBefore
		}
		if err := check(account, poster, bot, now); err != nil {
			t.Fatalf("crossed %s scope: %v", scope, err)
		}
	}
	// Another repository in the SAME configured account cannot spend a probe.
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	other := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "chat-other-target", "hosted", 2, 1))
	before := attemptCount(t, f.store, `SELECT count(*) FROM review_hosted_allowance_queries`)
	_, created, err := f.e.BeginHostedAllowanceQuery(t.Context(), otherAttemptProject, other.ID, 1, attemptAnchor(otherAttemptProject, 2), q.PostingActor, q.ProviderActor)
	assertChatHold(t, err)
	if created || attemptCount(t, f.store, `SELECT count(*) FROM review_hosted_allowance_queries`) != before {
		t.Fatal("shared cooldown created query opportunity")
	}
	// Existing intent remains recoverable, and never regains permission to post.
	replay, created, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, q.Anchor, q.PostingActor, q.ProviderActor)
	if err != nil || created || replay.ID != q.ID {
		t.Fatal("retained intent replay changed", created, err)
	}
	// Neither CLI quota nor source review credit is fabricated.
	if attemptCount(t, f.store, `SELECT count(*) FROM review_cli_preflight`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_operation_waits`) != 0 {
		t.Fatal("chat backoff created quota, credit, or automatic retry")
	}
}

func TestHostedChatCooldownRejectsCorruptRetainedReference(t *testing.T) {
	f, a, in := cooldownObservationFixture(t)
	in.Observation.Reason = reviewprovider.HostedChatCooldownReason
	if err := f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal(err)
	}
	q, err := f.e.ReadHostedAllowanceQuery(t.Context(), attemptProject, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `UPDATE review_hosted_observations SET capture_sha256=? WHERE attempt_id=?`, "broken", a.ID)
	err = f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return checkHostedChatCooldown(ctx, tx, a.Provider, a.Account, q.PostingActor, q.ProviderActor, time.Now().UTC())
	})
	if err == nil {
		t.Fatal("corrupt capture-bound cooldown became permission")
	}
}

func TestHostedChatCooldownBlocksAlreadyPreparedFullReview(t *testing.T) {
	f, a, _ := hostedActionFixture(t)
	own, err := f.e.ReadHostedAllowanceQuery(t.Context(), attemptProject, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	other := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "other-chat-reply", "hosted", 2, 1))
	anchor := attemptAnchor(otherAttemptProject, 2)
	q, _, err := f.e.BeginHostedAllowanceQuery(t.Context(), otherAttemptProject, other.ID, 1, anchor, own.PostingActor, own.ProviderActor)
	if err != nil {
		t.Fatal(err)
	}
	q, err = f.e.ConfirmHostedAllowanceQuery(t.Context(), otherAttemptProject, other.ID, 1, hostedQueryComment(t, q))
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC()
	in := HostedAllowanceInput{QueryID: q.ID, Anchor: anchor, ObservedAt: observed.Format(time.RFC3339Nano), ChangedFiles: 1, CaptureID: "review_capture_" + strings.Repeat("f", 32), CaptureSHA256: strings.Repeat("f", 64), CaptureBytes: 1000, Observation: reviewprovider.HostedAllowanceObservation{State: "cooldown_observed", Reason: reviewprovider.HostedChatCooldownReason, QueryCommentID: q.Comment.ID, ResponseCommentID: 92, QueryCreatedAt: q.Comment.CreatedAt, ResponseCreatedAt: q.Comment.CreatedAt.Add(time.Nanosecond), RetryNotBefore: q.Comment.CreatedAt.Add(time.Nanosecond).Add(3 * time.Minute)}}
	start, _ := time.Parse(time.RFC3339Nano, q.EarliestPosting)
	_, err = f.e.RecordHostedObservation(t.Context(), otherAttemptProject, other.ID, HostedObservationInput{Kind: "allowance_query", CaptureID: in.CaptureID, CaptureSHA256: in.CaptureSHA256, CaptureBytes: in.CaptureBytes, StartedAt: start.Truncate(time.Second).Format(time.RFC3339Nano), FinishedAt: in.ObservedAt, ObservedAt: in.ObservedAt, ExpectedAnchor: anchor, Intent: q.Intent})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.e.RecordHostedCooldown(t.Context(), otherAttemptProject, other.ID, 1, in); err != nil {
		t.Fatal(err)
	}
	before := hostedAllowanceState(t, f, a)
	assertChatHold(t, f.e.VerifyHostedAction(t.Context(), attemptProject, a.ID, a.Revision, hostedActionSource(a)))
	if hostedAllowanceState(t, f, a) != before {
		t.Fatal("negative final guard changed original intent or evidence")
	}
}

func TestHostedChatCooldownDoesNotConflateReviewAllowance(t *testing.T) {
	f, a, in := cooldownObservationFixture(t)
	if err := f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal(err)
	}
	q, err := f.e.ReadHostedAllowanceQuery(t.Context(), attemptProject, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return checkHostedChatCooldown(ctx, tx, a.Provider, a.Account, q.PostingActor, q.ProviderActor, time.Now().UTC())
	})
	if err != nil {
		t.Fatal("review cooldown was inferred to be a chat-message limit", err)
	}
	if _, err = f.e.ReadHostedAllowance(t.Context(), attemptProject, a.ID); err == nil {
		t.Fatal("negative cooldown became positive review allowance")
	}
}
