package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func stoppedQueuedFixture(t *testing.T, channel string) (*attemptFixture, Receipt) {
	t.Helper()
	f := newAttemptFixture(t)
	completionNoticeSchema(t, f)
	admissionSQL(t, f, `INSERT INTO workers VALUES(?, 'requester', 'Requester')`, attemptProject)
	f.configure(t, attemptProject, "test-capability", 0, false)
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "notice-baseline", encoded(reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: 801, Source: "stored-baseline", ObservedAt: stamp(), Queue: []reviews.PullInput{projectionPull(2), projectionPull(4)}}))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	raw, err := f.e.Enqueue(t.Context(), followingScope(t, f, "requester"), "stopped-request", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: channel, HeadSHA: strings.Repeat("a", 40)})
	var r Receipt
	if err != nil || json.Unmarshal(raw, &r) != nil {
		t.Fatal(err)
	}
	return f, r
}

func stoppedRead(t *testing.T, f *attemptFixture, id string) Operation {
	t.Helper()
	o, err := f.e.ReadOperation(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestStoppedNoticeExplicitRequesterOneAndDone(t *testing.T) {
	f, r := stoppedQueuedFixture(t, "cli")
	admissionSQL(t, f, `INSERT INTO workers VALUES(?, 'other-follower', 'Other')`, attemptProject)
	if _, err := f.e.ConfigureFollowing(t.Context(), followingScope(t, f, "other-follower"), "follow-other", 2, followingInput(0, true)); err != nil {
		t.Fatal(err)
	}
	driver := &controlledDriver{prepare: func(context.Context, Operation, Configuration, Attempt) (Preparation, error) {
		return Preparation{}, PreparationFailureAt("source_resolution", core.Fail("storage_unavailable", "/private/path ghp_secret provider body", 500))
	}}
	if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
		t.Fatal(err)
	}
	o := stoppedRead(t, f, r.ID)
	var id, recipient, body string
	if err := f.store.DB().QueryRow(`SELECT id,recipient_worker_id,body FROM inbox_messages`).Scan(&id, &recipient, &body); err != nil || recipient != "requester" || completionNoticeCount(t, f) != 1 || !strings.Contains(body, r.ID) || !strings.Contains(body, "before review dispatch") || strings.Contains(body, "ghp_secret") || strings.Contains(body, "private") || strings.Contains(body, "source_resolution") || o.State != "held" || driver.calls.Load() != 0 {
		t.Fatal("unsafe or misrouted stopped notice", err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	inbox.Register(mux, f.store)
	f.handler = mux
	response := f.request("POST", "/api/v1/projects/"+attemptProject+"/messages/"+id+"/ack", f.admin, "handle-stopped", `{"worker_id":"requester"}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	// Calling the producer again must preserve the original immutable message and receipt.
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return publishStoppedOperationNotice(ctx, tx, o, o.State, o.Reason, false)
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := f.e.Enqueue(t.Context(), followingScope(t, f, "requester"), "stopped-request", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	var replay Receipt
	if err != nil || json.Unmarshal(raw, &replay) != nil || replay.ID != r.ID || completionNoticeCount(t, f) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM inbox_recipient_state WHERE acknowledged=0`) != 0 {
		t.Fatal("replay duplicated or resurrected notice", err)
	}
}

func TestStoppedNoticeAutomaticUsesGenuineFollowers(t *testing.T) {
	f := schedulingFixture(t)
	completionNoticeSchema(t, f)
	admissionSQL(t, f, `INSERT INTO workers VALUES(?, 'participant', 'Participant')`, attemptProject)
	if _, err := f.e.ConfigureFollowing(t.Context(), followingScope(t, f, "participant"), "follow-front", 2, followingInput(0, true)); err != nil {
		t.Fatal(err)
	}
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	selected := schedulingLatest(t, f)
	a := f.start(t, attemptProject, Receipt{ID: selected.OperationID})
	if _, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "held", "preparation_failed", ""); err != nil {
		t.Fatal(err)
	}
	var recipient string
	if err := f.store.DB().QueryRow(`SELECT recipient_worker_id FROM inbox_messages`).Scan(&recipient); err != nil || recipient != "participant" || completionNoticeCount(t, f) != 1 || followingPreference(t, f, "selection-worker", 2).Mode != "none" {
		t.Fatal("automatic notice inferred authorizer or missed follower", err)
	}
}

func TestStoppedNoticePreAttemptGuardsAndDraftStop(t *testing.T) {
	for _, change := range []string{"configuration", "capability", "authorization", "policy", "draft"} {
		t.Run(change, func(t *testing.T) {
			f, r := stoppedQueuedFixture(t, "cli")
			switch change {
			case "configuration":
				f.configure(t, attemptProject, "other-account", 1, false)
			case "capability":
				delete(f.e.capabilities, "test-capability")
			case "authorization":
				admissionSQL(t, f, `UPDATE grants SET can_write=0 WHERE credential_id='exec-writer-cred'`)
			case "policy":
				admissionPolicy(t, f, 1)
			case "draft":
				if err := f.e.recordCLIDraftDecision(t.Context(), stoppedRead(t, f, r.ID), cliDraftWait); err != nil {
					t.Fatal(err)
				}
				if completionNoticeCount(t, f) != 0 {
					t.Fatal("draft wait notified")
				}
				if err := f.e.recordCLIDraftDecision(t.Context(), stoppedRead(t, f, r.ID), "repository_changed"); err != nil {
					t.Fatal(err)
				}
			}
			if change != "draft" {
				if a, err := f.e.StartAttempt(t.Context(), attemptProject, r.ID); err != nil || a.ID != "" {
					t.Fatal("guard created attempt", err)
				}
			}
			o := stoppedRead(t, f, r.ID)
			if o.State != "held" || o.AttemptID != "" || completionNoticeCount(t, f) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
				t.Fatal("pre-attempt stop did not notify", change, o)
			}
		})
	}
}

func TestStoppedNoticeFailureRestartReportsOnlyInterruptedPreparation(t *testing.T) {
	f, r := stoppedQueuedFixture(t, "cli")
	admissionSQL(t, f, `CREATE TRIGGER reject_stopped_notice BEFORE INSERT ON inbox_messages BEGIN SELECT RAISE(ABORT,'private injected inbox failure'); END`)
	driver := &controlledDriver{prepare: func(context.Context, Operation, Configuration, Attempt) (Preparation, error) {
		return Preparation{}, PreparationFailureAt("attempt_publication", core.Fail("storage_unavailable", "private original failure", 500))
	}}
	err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver})
	attemptCode(t, err, "review_persistence_failed")
	original := stoppedRead(t, f, r.ID)
	if original.State != "preparing" || original.Reason != "" || completionNoticeCount(t, f) != 0 || driver.calls.Load() != 0 {
		t.Fatal("failed insertion falsely retained original result", original)
	}
	admissionSQL(t, f, `DROP TRIGGER reject_stopped_notice`)
	var seq int
	var name, database string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &database); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(t.Context(), filepath.Dir(database))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	f.store = reopened
	f.e, err = New(reopened, attemptCapabilities, WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	if err = f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	recovered := stoppedRead(t, f, r.ID)
	var body string
	if err = f.store.DB().QueryRow(`SELECT body FROM inbox_messages`).Scan(&body); err != nil || recovered.State != "cancelled" || recovered.Reason != "supervisor_interrupted" || !strings.Contains(body, "original preparation outcome is unavailable") || strings.Contains(body, "attempt_publication") || strings.Contains(body, "private") || completionNoticeCount(t, f) != 1 {
		t.Fatal("restart invented original failure or lost interruption notice", recovered, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || driver.calls.Load() != 0 {
		t.Fatal("interrupted preparation dispatched or kept reservation")
	}
	if err = f.e.RecoverInterrupted(t.Context()); err != nil || completionNoticeCount(t, f) != 1 {
		t.Fatal("restart duplicated notice", err)
	}
}

func TestStoppedNoticeQuietReasonsAndPreferences(t *testing.T) {
	for _, tc := range []struct{ state, reason string }{{"cancelled", "supervisor_stopping"}, {"cancelled", "operator_cancelled"}, {"held", cliDraftWait}, {"held", "target_already_covered"}, {"held", "review_round_closed"}, {"held", "review_round_complete"}, {"held", "review_round_capacity"}, {"failed", "cli_partial_settled"}, {"held", "restore_reconciled"}, {"held", "target_merged"}, {"held", "target_closed"}, {"held", "target_superseded"}} {
		if stoppedOperationNeedsNotice(tc.state, tc.reason, false) {
			t.Fatal("quiet outcome would notify", tc)
		}
	}
	for _, mode := range []string{"muted", "retired", "standalone", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f, a := completionNoticeStartedFixture(t, "cli")
			state, reason := "held", "preparation_failed"
			switch mode {
			case "muted":
				if _, err := f.e.ConfigureFollowing(t.Context(), followingScope(t, f, "requester"), "mute-stops", 2, followingInput(1, false)); err != nil {
					t.Fatal(err)
				}
			case "retired":
				admissionSQL(t, f, `INSERT INTO worker_retirements VALUES(?,'requester',?,'fixture')`, a.ProjectID, stamp())
			case "standalone":
				var err error
				f.e, err = New(f.store, attemptCapabilities)
				if err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				state, reason = "cancelled", "supervisor_stopping"
			}
			if _, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, state, reason, ""); err != nil || completionNoticeCount(t, f) != 0 {
				t.Fatal("quiet or excluded recipient notified", mode, err)
			}
		})
	}
}

func TestStoppedNoticeAdmissionRoundClosureStaysQuiet(t *testing.T) {
	f, r := stoppedQueuedFixture(t, "cli")
	// The operation preceded the grant, so start a new original request after the
	// first queued one is deliberately cancelled without a stopped notification.
	timerControl(t, f, r.ID, "cancel", "replace-pregrant-request")
	roundGrant(t, f, 2, "cli", 0, 0, 4, "available-rounds")
	raw, err := f.e.Enqueue(t.Context(), followingScope(t, f, "requester"), "round-close-request", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	var receipt Receipt
	if err != nil || json.Unmarshal(raw, &receipt) != nil {
		t.Fatal(err)
	}
	driver := &controlledDriver{prepare: func(ctx context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
		revision := int64(1)
		if _, err := f.e.ConfigureRoundControl(ctx, f.scope(t, attemptProject, f.admin, true), "close-during-prepare", 2, "cli", RoundControlRequest{ExpectedRevision: &revision, Action: "close"}); err != nil {
			t.Fatal(err)
		}
		return Preparation{Anchor: attemptAnchor(o.ProjectID, o.PullNumber)}, nil
	}}
	if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
		t.Fatal(err)
	}
	o := stoppedRead(t, f, receipt.ID)
	if o.State != "held" || o.Reason != "review_round_closed" || completionNoticeCount(t, f) != 0 || driver.calls.Load() != 0 {
		t.Fatal("round closure lost classification or notified", o)
	}
}

func TestStoppedNoticeObserverHoldAndOperatorSettlement(t *testing.T) {
	for _, notify := range []bool{false, true} {
		t.Run(map[bool]string{false: "operator", true: "observer"}[notify], func(t *testing.T) {
			f, r := stoppedQueuedFixture(t, "hosted")
			if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				return holdQueuedExternalHosted(ctx, tx, attemptProject, 2, notify)
			}); err != nil {
				t.Fatal(err)
			}
			want := 0
			if notify {
				want = 1
			}
			if stoppedRead(t, f, r.ID).State != "held" || completionNoticeCount(t, f) != want {
				t.Fatal("observer/operator distinction lost")
			}
		})
	}
}

func TestStoppedNoticeInterruptedIssuedWorkDoesNotClaimPreparation(t *testing.T) {
	f, a := captureAttentionFixture(t, "cli")
	if err := f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	if o := stoppedRead(t, f, a.OperationID); o.State != "uncertain" || completionNoticeCount(t, f) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 {
		t.Fatal("issued work claimed unused preparation", o)
	}
}

func TestStoppedNoticeProvenObsoleteTargetAndUnknownChange(t *testing.T) {
	for _, kind := range []string{"merged", "closed", "superseded", "invalid_identity"} {
		t.Run(kind, func(t *testing.T) {
			f, r := stoppedQueuedFixture(t, "cli")
			reader := &refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) {
				p := reviewprovider.Pull{Number: 2, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}
				switch kind {
				case "merged":
					p.Merged = true
					p.State = "closed"
				case "closed":
					p.State = "closed"
				case "superseded":
					p.HeadSHA = strings.Repeat("c", 40)
				case "invalid_identity":
					p.RepositoryID = 999
					p.State = "closed"
				}
				return p, nil
			}}
			driver := &controlledDriver{prepare: func(ctx context.Context, o Operation, c Configuration, _ Attempt) (Preparation, error) {
				_, err := ResolveSource(ctx, reader, 801, c, Request{PullNumber: o.PullNumber, HeadSHA: o.HeadSHA, Channel: o.Channel})
				return Preparation{}, err
			}}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
				t.Fatal(err)
			}
			o := stoppedRead(t, f, r.ID)
			wantReason := "target_" + kind
			want := 0
			if kind == "invalid_identity" {
				wantReason = "target_changed"
				want = 1
			}
			if o.State != "held" || o.Reason != wantReason || completionNoticeCount(t, f) != want || driver.calls.Load() != 0 {
				t.Fatal("obsolete classification inferred unsupported facts or became noisy", kind, o)
			}
		})
	}
}

func TestStoppedNoticeAcknowledgmentSurvivesRestart(t *testing.T) {
	f, a := completionNoticeStartedFixture(t, "cli")
	if _, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "failed", "private_token_shaped_reason", ""); err != nil {
		t.Fatal(err)
	}
	var id, body string
	if err := f.store.DB().QueryRow(`SELECT id,body FROM inbox_messages`).Scan(&id, &body); err != nil || strings.Contains(body, "private_token") {
		t.Fatal("unsafe failure notice", err)
	}
	mux := http.NewServeMux()
	inbox.Register(mux, f.store)
	f.handler = mux
	response := f.request("POST", "/api/v1/projects/"+attemptProject+"/messages/"+id+"/ack", f.admin, "handled-before-restart", `{"worker_id":"requester"}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var seq int
	var name, database string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &database); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(t.Context(), filepath.Dir(database))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	f.store = reopened
	f.e, err = New(reopened, attemptCapabilities, WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	if err = f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	o := stoppedRead(t, f, a.OperationID)
	if err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return publishStoppedOperationNotice(ctx, tx, o, o.State, o.Reason, false)
	}); err != nil {
		t.Fatal(err)
	}
	if completionNoticeCount(t, f) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM inbox_recipient_state WHERE acknowledged=0`) != 0 {
		t.Fatal("restart/reproduction resurrected acknowledged notice")
	}
}

func TestStoppedNoticeTypedPreparationRateLimitIsActionable(t *testing.T) {
	f, r := stoppedQueuedFixture(t, "cli")
	failure := &reviewprovider.Error{Code: "provider_rate_limited", Status: 429, Message: "private provider response"}
	for _, stage := range []string{"execution_context", "source_resolution", "workspace", "retry_baseline", "source_refresh", "allowance_recording", "attempt_publication", "allowance_wait", "trigger_intent", "unknown"} {
		reason := preparationReason(PreparationFailureAt(stage, failure))
		if !stoppedOperationNeedsNotice("held", reason, false) {
			t.Fatal("terminal typed rate limit was hidden", stage, reason)
		}
	}
	if stoppedOperationNeedsNotice("queued", "preparation_source_resolution_provider_rate_limited", false) {
		t.Fatal("a queued retry notified")
	}
	driver := &controlledDriver{prepare: func(context.Context, Operation, Configuration, Attempt) (Preparation, error) {
		return Preparation{}, PreparationFailureAt("source_resolution", failure)
	}}
	if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
		t.Fatal(err)
	}
	o := stoppedRead(t, f, r.ID)
	if o.State != "held" || o.Reason != "preparation_source_resolution_provider_rate_limited" || completionNoticeCount(t, f) != 1 || driver.calls.Load() != 0 {
		t.Fatal("source 429 was lost, dispatched or left unnotified", o)
	}
	for _, reason := range []string{"pull_draft", "target_already_covered"} {
		if stoppedOperationNeedsNotice("failed", reason, false) {
			t.Fatal("final-boundary quiet class notified", reason)
		}
	}
}

func TestStoppedNoticeTerminalWaitingReasonsAreActionable(t *testing.T) {
	for _, state := range []string{"held", "failed", "rate_limited"} {
		for _, reason := range []string{"provider_rate_limited", "provider_activity_rate_limited", "hosted_allowance_reply_pending", "allowance_exhausted", "hosted_allowance_cooldown"} {
			t.Run(state+"_"+reason, func(t *testing.T) {
				f, a := completionNoticeStartedFixture(t, "cli")
				if _, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, state, reason, ""); err != nil {
					t.Fatal(err)
				}
				o := stoppedRead(t, f, a.OperationID)
				if o.State != state || o.Wait != nil || completionNoticeCount(t, f) != 1 {
					t.Fatal("terminal waiting-sounding result left requester waiting", o)
				}
			})
		}
	}
}

func TestStoppedNoticeDurableQueuedQuotaRetriesStayQuiet(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f, a := completionNoticeStartedFixture(t, channel)
			if channel == "cli" {
				input := CLIAllowanceObservation{Anchor: attemptAnchor(attemptProject, 2), ObservedAt: stamp(), Remaining: 0, Limit: 3, FullCapacityInMS: 60000, RollingWindowMS: 60000, ChangedFiles: 2}
				if err := f.e.RecordCLIExhaustion(t.Context(), a.ProjectID, a.ID, a.Revision, input); err != nil {
					t.Fatal(err)
				}
				if _, err := f.e.ScheduleCLICooldown(t.Context(), a.ProjectID, a.ID, a.Revision); err != nil {
					t.Fatal(err)
				}
			} else {
				retainTimerCooldown(t, f, a, 3*time.Minute)
				if _, err := f.e.ScheduleHostedCooldown(t.Context(), a.ProjectID, a.ID, a.Revision); err != nil {
					t.Fatal(err)
				}
			}
			o := stoppedRead(t, f, a.OperationID)
			if o.State != "queued" || o.Wait == nil || o.Wait.RetryNotBefore == "" || completionNoticeCount(t, f) != 0 {
				t.Fatal("durable quota retry became a stopped notice", o)
			}
			if err := f.e.RecoverInterrupted(t.Context()); err != nil || completionNoticeCount(t, f) != 0 {
				t.Fatal("restart notified queued retry", err)
			}
		})
	}
}
