package reviewexec

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func captureAttentionFixture(t *testing.T, channel string) (*attemptFixture, Attempt) {
	t.Helper()
	f, a := completionNoticeStartedFixture(t, channel)
	anchor := attemptAnchor(a.ProjectID, 2)
	anchor.ParentSHA = anchor.BaseSHA
	intent := ""
	if channel == "hosted" {
		var err error
		intent, err = reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: anchor.CandidateSHA})
		if err != nil {
			t.Fatal(err)
		}
	}
	var err error
	a, err = f.e.Admit(t.Context(), a.ProjectID, a.ID, a.Revision, anchor, intent)
	if err != nil {
		t.Fatal(err)
	}
	if channel == "hosted" {
		a, err = f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "active", "posted", "101")
		if err != nil {
			t.Fatal(err)
		}
	}
	return f, a
}

func TestCaptureAttentionNoticeOriginalIdentityNoCreditAndRestartDedup(t *testing.T) {
	f, a := captureAttentionFixture(t, "hosted")
	admissionSQL(t, f, `UPDATE projects SET repository='renamed/current' WHERE id=?`, a.ProjectID)
	capability := f.e.capabilities["test-capability"]
	capability.Provider = "different-provider"
	f.e.capabilities["test-capability"] = capability
	blocked, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "uncertain", "hosted_finished_inventory_unproven", "")
	if err != nil || completionNoticeCount(t, f) != 1 {
		t.Fatal("new block did not notify requester", err)
	}
	var body, recipient string
	if err := f.store.DB().QueryRow(`SELECT body,recipient_worker_id FROM inbox_messages`).Scan(&body, &recipient); err != nil || recipient != "requester" || !strings.HasPrefix(body, "CodeRabbit Hosted capture needs attention") || !strings.Contains(body, "https://github.com/owner/repo/pull/2") || !strings.Contains(body, a.OperationID) || strings.Contains(body, "renamed/current") || strings.Contains(body, "different-provider") {
		t.Fatal("notice lost original binding", err)
	}
	for _, table := range []string{"review_execution_evidence", "review_round_completions"} {
		if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("attention notice invented evidence or credit", table)
		}
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 1 {
		t.Fatal("attention notice released uncertain reservation")
	}
	// Replaying the original request after the notification returns the original
	// operation, without sending a review or issuing another attention message.
	raw, err := f.e.Enqueue(t.Context(), followingScope(t, f, "requester"), "explicit-review-request", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "hosted", HeadSHA: strings.Repeat("a", 40)})
	var receipt Receipt
	if err != nil || json.Unmarshal(raw, &receipt) != nil || receipt.ID != a.OperationID || completionNoticeCount(t, f) != 1 {
		t.Fatal("original request replay duplicated work or notice", err)
	}
	for range 3 {
		check := f.e.BeginHostedObserver("test-capability", blocked.ProjectID, blocked.OperationID, blocked.ID)
		f.e.FinishHostedObserver(check, time.Now().UTC(), time.Time{}, "hosted_finished_inventory_unproven", nil)
	}
	if completionNoticeCount(t, f) != 1 {
		t.Fatal("unchanged observer passes duplicated attention notice")
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
	// Unchanged observer passes do not transition at all. A true active reentry
	// reaching the same original operation/reason still uses one durable key.
	active, err := f.e.Advance(t.Context(), blocked.ProjectID, blocked.ID, blocked.Revision, "active", "trigger_recovered", "101")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.Advance(t.Context(), active.ProjectID, active.ID, active.Revision, "uncertain", "hosted_finished_inventory_unproven", ""); err != nil || completionNoticeCount(t, f) != 1 {
		t.Fatal("restart/reentry duplicated notice", err)
	}
}

func TestCaptureAttentionNoticeFailureRollsBackTransition(t *testing.T) {
	f, a := captureAttentionFixture(t, "hosted")
	admissionSQL(t, f, `CREATE TRIGGER reject_capture_attention BEFORE INSERT ON inbox_messages BEGIN SELECT RAISE(ABORT,'injected message failure'); END`)
	before := evidenceState(t, f, a) + projectionDigest(t, f, "")
	if _, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "uncertain", "hosted_finished_inventory_unproven", ""); err == nil || before != evidenceState(t, f, a)+projectionDigest(t, f, "") {
		t.Fatal("notice failure partially settled capture block", err)
	}
	for _, table := range []string{"inbox_messages", "inbox_conversations", "inbox_recipient_state"} {
		if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("notice failure partially wrote inbox", table)
		}
	}
	admissionSQL(t, f, `DROP TRIGGER reject_capture_attention`)
	if _, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "uncertain", "hosted_finished_inventory_unproven", ""); err != nil || completionNoticeCount(t, f) != 1 {
		t.Fatal("original transition could not retry", err)
	}
}

func TestCaptureAttentionNoticeOnlyActionableHostedBlock(t *testing.T) {
	for _, test := range []struct{ channel, reason string }{{"hosted", "dispatch_interrupted"}, {"hosted", "observation_pending"}, {"hosted", "provider_unavailable"}, {"cli", "hosted_finished_inventory_unproven"}} {
		t.Run(test.channel+test.reason, func(t *testing.T) {
			f, a := captureAttentionFixture(t, test.channel)
			if _, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "uncertain", test.reason, ""); err != nil || completionNoticeCount(t, f) != 0 {
				t.Fatal("nonactionable uncertainty produced attention spam", err)
			}
		})
	}
	for _, mode := range []string{"muted", "retired", "standalone"} {
		t.Run(mode, func(t *testing.T) {
			f, a := captureAttentionFixture(t, "hosted")
			if mode == "muted" {
				if _, err := f.e.ConfigureFollowing(t.Context(), followingScope(t, f, "requester"), "mute", 2, followingInput(1, false)); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "retired" {
				admissionSQL(t, f, `INSERT INTO worker_retirements VALUES(?,'requester',?,'fixture')`, a.ProjectID, stamp())
			}
			if mode == "standalone" {
				var err error
				f.e, err = New(f.store, attemptCapabilities)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "uncertain", "hosted_finished_inventory_unproven", ""); err != nil || completionNoticeCount(t, f) != 0 {
				t.Fatal("attention notification ignored routing boundary", err)
			}
		})
	}
}

func TestCaptureAttentionNoticeDoesNotSuppressCompletionReady(t *testing.T) {
	f, a, _ := completionNoticeFixture(t, "hosted", true)
	blocked, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "uncertain", "hosted_finished_inventory_unproven", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.Advance(t.Context(), blocked.ProjectID, blocked.ID, blocked.Revision, "completed", "hosted_evidence_recovered", ""); err != nil || completionNoticeCount(t, f) != 2 {
		t.Fatal("attention notice suppressed recovered completion notice", err)
	}
}

func TestCaptureAttentionNoticeAutomaticUsesExistingParticipants(t *testing.T) {
	f := schedulingFixture(t)
	completionNoticeSchema(t, f)
	admissionSQL(t, f, `INSERT INTO workers VALUES(?,?,?)`, attemptProject, "participant", "Actual participant")
	if _, err := f.e.ConfigureFollowing(t.Context(), followingScope(t, f, "participant"), "follow", 2, followingInput(0, true)); err != nil {
		t.Fatal(err)
	}
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	selected := schedulingLatest(t, f)
	a := f.start(t, attemptProject, Receipt{ID: selected.OperationID})
	a, _ = completionNoticeCapture(t, f, a, true)
	if _, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "uncertain", "hosted_finished_inventory_unproven", ""); err != nil || completionNoticeCount(t, f) != 1 {
		t.Fatal("automatic capture block did not notify participant", err)
	}
	var recipient string
	if err := f.store.DB().QueryRow(`SELECT recipient_worker_id FROM inbox_messages`).Scan(&recipient); err != nil || recipient != "participant" {
		t.Fatal("automatic authorizer became inferred recipient", err)
	}
}
