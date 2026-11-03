package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

func completionNoticeSchema(t *testing.T, f *attemptFixture) {
	t.Helper()
	for _, ensure := range []func(context.Context, *core.Store) error{reviews.EnsureSchema, inbox.EnsureSchema, EnsureEvidenceSchema, EnsureAdjudicationSchema} {
		if err := ensure(t.Context(), f.store); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	f.e, err = New(f.store, attemptCapabilities, WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, f.store)
	f.handler = mux
}

func completionNoticeCapture(t *testing.T, f *attemptFixture, a Attempt, complete bool) (Attempt, Receipt) {
	t.Helper()
	o, err := f.e.ReadOperation(t.Context(), evidenceReadScope(t, f, a.ProjectID, f.reader), a.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	anchor := attemptAnchor(a.ProjectID, o.PullNumber)
	intent := ""
	if o.Channel == "hosted" {
		anchor.ParentSHA = anchor.BaseSHA
		intent, err = reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: anchor.CandidateSHA})
		if err != nil {
			t.Fatal(err)
		}
	}
	a, err = f.e.Admit(t.Context(), a.ProjectID, a.ID, a.Revision, anchor, intent)
	if err != nil {
		t.Fatal(err)
	}
	if o.Channel == "hosted" {
		a, err = f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "active", "posted", "101")
		if err != nil {
			t.Fatal(err)
		}
	}
	digest := strings.Repeat("1", 64)
	in := ReviewEvidenceInput{CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1024, StartedAt: time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano), FinishedAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), ExpectedAnchor: anchor, Review: reviewprovider.ParsedReview{Outcome: "completed", Complete: true, ReviewedFiles: []string{"changed.go"}, Findings: []reviewprovider.ParsedFinding{}}}
	if o.Channel == "hosted" {
		metadata, _, parsed := hostedEvidenceMetadataFixture()
		metadata.ObservedBaseSHA = anchor.ParentSHA
		for index := range parsed.Findings {
			parsed.Findings[index].Title = "Retained Hosted finding"
			parsed.Findings[index].Detail = "Synthetic immutable source finding."
			parsed.Findings[index].Severity = "medium"
		}
		in.Hosted, in.Review = metadata, parsed
	}
	if !complete {
		in.Review.Outcome, in.Review.Complete = "failed", false
	}
	r, err := f.e.RecordReviewEvidence(t.Context(), a.ProjectID, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	return a, r
}

func completionNoticeFixture(t *testing.T, channel string, complete bool) (*attemptFixture, Attempt, Receipt) {
	t.Helper()
	f := newAttemptFixture(t)
	completionNoticeSchema(t, f)
	admissionSQL(t, f, `INSERT INTO workers VALUES(?,?,?)`, attemptProject, "requester", "Requester")
	f.configure(t, attemptProject, "test-capability", 0, false)
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "initial-stored-pr", encoded(reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: 801, Source: "stored-baseline", ObservedAt: stamp(), Queue: []reviews.PullInput{projectionPull(2), projectionPull(4)}}))
	if w.Code != 201 {
		t.Fatal("initial stored metadata unavailable", w.Code, w.Body.String())
	}
	scope := followingScope(t, f, "requester")
	raw, err := f.e.Enqueue(t.Context(), scope, "explicit-review-request", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: channel, HeadSHA: strings.Repeat("a", 40)})
	var receipt Receipt
	if err != nil || json.Unmarshal(raw, &receipt) != nil || followingPreference(t, f, "requester", 2).Mode != "auto" {
		t.Fatal("explicit requester was not retained as participant", err)
	}
	a := f.start(t, attemptProject, receipt)
	a, evidence := completionNoticeCapture(t, f, a, complete)
	return f, a, evidence
}

func completionNoticeCount(t *testing.T, f *attemptFixture) int {
	t.Helper()
	return attemptCount(t, f.store, `SELECT count(*) FROM inbox_messages WHERE origin_kind='controller'`)
}

func TestCompletionNoticeCLIAndHostedWaitForUsableCompletion(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f, a, evidence := completionNoticeFixture(t, channel, true)
			if completionNoticeCount(t, f) != 0 {
				t.Fatal("request, trigger or raw capture sent a completion message")
			}
			if _, err := f.e.PublishEvidence(t.Context(), a.ProjectID, evidence.ID, nil); err != nil {
				t.Fatal(err)
			}
			if completionNoticeCount(t, f) != 0 {
				t.Fatal("publication before settlement claimed completion")
			}
			completed, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "completed", "fixture_review_completed", "")
			if err != nil || completed.State != "completed" || completionNoticeCount(t, f) != 1 {
				t.Fatal("completed result did not notify exactly once", completed, err)
			}
			var body, recipient, origin string
			var sender sql.NullString
			if err := f.store.DB().QueryRow(`SELECT body,recipient_worker_id,origin_kind,sender_worker_id FROM inbox_messages`).Scan(&body, &recipient, &origin, &sender); err != nil || recipient != "requester" || origin != "controller" || sender.Valid || !strings.Contains(body, evidence.ID) || !strings.Contains(body, a.OperationID) || !strings.Contains(body, "PR #2") || !strings.HasPrefix(body, "CodeRabbit ") || !strings.Contains(body, "GitHub: https://github.com/owner/repo/pull/2\n") || !strings.Contains(body, map[string]string{"cli": "CLI", "hosted": "Hosted"}[channel]) {
				t.Fatal("notice lost immutable identity or impersonated a worker", body, err)
			}
			if n := attemptCount(t, f.store, `SELECT count(*) FROM inbox_recipient_state WHERE seen<>0 OR acknowledged<>0`); n != 0 {
				t.Fatal("notification changed handling state")
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='message.sent'`) != 1 {
				t.Fatal("completion did not publish its durable inbox event")
			}
			_, err = f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "completed", "fixture_review_completed", "")
			attemptCode(t, err, "review_conflict")
			if _, err := f.e.PublishEvidence(t.Context(), a.ProjectID, evidence.ID, nil); err != nil || completionNoticeCount(t, f) != 1 {
				t.Fatal("publication replay duplicated notice", err)
			}
			state := adjudicationRead(t, f, evidence.ID)
			if state.Finalized || state.Revision != 0 {
				t.Fatal("notification adjudicated the review", state)
			}
		})
	}
}

func TestCompletionNoticeRecoveryRestartAndReplay(t *testing.T) {
	f, a, evidence := completionNoticeFixture(t, "cli", true)
	a, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "uncertain", "capture_publication_pending", "")
	if err != nil {
		t.Fatal(err)
	}
	var sequence int
	var name, database string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &database); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(t.Context(), filepath.Dir(database))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	f.store = reopened
	f.e, err = New(reopened, attemptCapabilities, WithNativeProjection())
	if err != nil || completionNoticeCount(t, f) != 0 {
		t.Fatal("restart scanned or notified unfinished history", err)
	}
	if err := f.e.RecoverRetainedCLI(t.Context(), "test-capability"); err != nil || completionNoticeCount(t, f) != 1 {
		t.Fatal("retained exact CLI recovery did not notify", err)
	}
	if err := f.e.RecoverRetainedCLI(t.Context(), "test-capability"); err != nil || completionNoticeCount(t, f) != 1 {
		t.Fatal("recovery replay duplicated message", err)
	}
	if _, err := f.e.PublishEvidence(t.Context(), a.ProjectID, evidence.ID, nil); err != nil || completionNoticeCount(t, f) != 1 {
		t.Fatal("post-restart publication replay duplicated message", err)
	}
}

func TestCompletionNoticeMessageFailureRollsBackSettlement(t *testing.T) {
	f, a, _ := completionNoticeFixture(t, "cli", true)
	admissionSQL(t, f, `CREATE TRIGGER reject_ready_message BEFORE INSERT ON inbox_messages BEGIN SELECT RAISE(ABORT,'injected message failure'); END`)
	before := evidenceState(t, f, a) + projectionDigest(t, f, "")
	if _, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "completed", "fixture_review_completed", ""); err == nil || completionNoticeCount(t, f) != 0 || before != evidenceState(t, f, a)+projectionDigest(t, f, "") {
		t.Fatal("failed message left a settled attempt, event or partial projection", err)
	}
	for _, table := range []string{"inbox_conversations", "inbox_recipient_state"} {
		if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("failed notification left partial inbox state", table)
		}
	}
	admissionSQL(t, f, `DROP TRIGGER reject_ready_message`)
	if _, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "completed", "fixture_review_completed", ""); err != nil || completionNoticeCount(t, f) != 1 {
		t.Fatal("same original completion could not retry atomically", err)
	}
}

func TestCompletionNoticeConcurrentSettlementDeliversOnce(t *testing.T) {
	f, a, _ := completionNoticeFixture(t, "cli", true)
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "completed", "fixture_completed", "")
			results <- err
		}()
	}
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		} else {
			attemptCode(t, err, "review_conflict")
		}
	}
	if successes != 1 || completionNoticeCount(t, f) != 1 {
		t.Fatal("concurrent completion duplicated settlement or delivery", successes)
	}
}

func TestCompletionNoticeIncompleteAndOtherTerminalStatesStaySilent(t *testing.T) {
	for _, state := range []string{"completed", "failed", "cancelled", "rate_limited"} {
		t.Run(state, func(t *testing.T) {
			f, a, _ := completionNoticeFixture(t, "cli", false)
			if _, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, state, "fixture_terminal", ""); err != nil || completionNoticeCount(t, f) != 0 {
				t.Fatal("incomplete capture or terminal diagnostics claimed ready evidence", err)
			}
		})
	}
}

func TestCompletionNoticeStandaloneAndHistoricalImportStaySilent(t *testing.T) {
	f, a, _ := completionNoticeFixture(t, "cli", true)
	standalone, err := New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := standalone.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "completed", "fixture_standalone", ""); err != nil || completionNoticeCount(t, f) != 0 {
		t.Fatal("standalone journal published a workspace notice", err)
	}
	f.e, err = New(f.store, attemptCapabilities, WithNativeProjection())
	if err != nil || completionNoticeCount(t, f) != 0 {
		t.Fatal("enabling projection scanned old completions", err)
	}
	var revision int64
	if err := f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, a.ProjectID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	pull := projectionPull(2)
	pull.Runs = []reviews.RunInput{{SourceRunID: "imported-completed-run", Channel: "cli", Scope: "retained history", Outcome: "completed", Complete: true, Finalized: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z"}}
	w := f.request("POST", "/api/v1/projects/"+a.ProjectID+"/reviews/observations", f.admin, "retained-import", encoded(reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 801, Source: "native-executor", ObservedAt: stamp(), Queue: []reviews.PullInput{pull, projectionPull(4)}}))
	if w.Code != 201 || completionNoticeCount(t, f) != 0 {
		t.Fatal("historical producer label sent live completion notification", w.Code, w.Body.String())
	}
}

func TestCompletionNoticeAutomaticUsesParticipantsNotProjectAuthorizer(t *testing.T) {
	f := schedulingFixture(t)
	completionNoticeSchema(t, f)
	admissionSQL(t, f, `INSERT INTO workers VALUES(?,?,?)`, attemptProject, "participant", "Actual participant")
	if _, err := f.e.ConfigureFollowing(t.Context(), followingScope(t, f, "participant"), "follow-automatic-pr", 2, followingInput(0, true)); err != nil {
		t.Fatal(err)
	}
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	selected := schedulingLatest(t, f)
	if selected.State != "selected" || followingPreference(t, f, "selection-worker", 2).Mode != "none" {
		t.Fatal("automatic authorizer became a PR participant", selected)
	}
	a := f.start(t, attemptProject, Receipt{ID: selected.OperationID})
	a, _ = completionNoticeCapture(t, f, a, true)
	if _, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "completed", "fixture_automatic_completed", ""); err != nil || completionNoticeCount(t, f) != 1 {
		t.Fatal("automatic completed review did not notify its participant", err)
	}
	var recipient, actor string
	if err := f.store.DB().QueryRow(`SELECT m.recipient_worker_id,e.actor_id FROM inbox_messages m JOIN events e ON e.resource_id=m.id WHERE e.event_type='message.sent'`).Scan(&recipient, &actor); err != nil || recipient != "participant" || actor != "exec-writer-principal" {
		t.Fatal("automatic notice inferred an owner or changed actor provenance", recipient, actor, err)
	}
}

func TestCompletionNoticeRequesterMuteAndRetirementStayEffective(t *testing.T) {
	for _, mode := range []string{"muted", "retired"} {
		t.Run(mode, func(t *testing.T) {
			f, a, _ := completionNoticeFixture(t, "cli", true)
			if mode == "muted" {
				if _, err := f.e.ConfigureFollowing(t.Context(), followingScope(t, f, "requester"), "mute-own-request", 2, followingInput(1, false)); err != nil {
					t.Fatal(err)
				}
			} else {
				admissionSQL(t, f, `INSERT INTO worker_retirements VALUES(?,'requester',?,'fixture')`, a.ProjectID, stamp())
			}
			if _, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "completed", "fixture_completed", ""); err != nil || completionNoticeCount(t, f) != 0 {
				t.Fatal("completion ignored requester notification preference or retirement", err)
			}
		})
	}
}

func TestCompletionNoticeUsesOriginalProviderAndRepository(t *testing.T) {
	f, a, _ := completionNoticeFixture(t, "hosted", true)
	// Rename current project metadata and change the configured adapter after the
	// original attempt admitted its immutable comparison.
	admissionSQL(t, f, `UPDATE projects SET repository='renamed/current' WHERE id=?`, a.ProjectID)
	capability := f.e.capabilities["test-capability"]
	capability.Provider = "different-provider"
	f.e.capabilities["test-capability"] = capability
	if _, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "completed", "fixture_completed", ""); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := f.store.DB().QueryRow(`SELECT body FROM inbox_messages`).Scan(&body); err != nil || !strings.HasPrefix(body, "CodeRabbit Hosted review results ready") || !strings.Contains(body, "https://github.com/owner/repo/pull/2") || strings.Contains(body, "renamed/current") || strings.Contains(body, "different-provider") {
		t.Fatal("notice substituted current configuration for original identity", body, err)
	}
}

func TestCompletedReviewContextValidatesOriginalIdentity(t *testing.T) {
	for _, tc := range []struct {
		provider, repository string
		number               int64
		valid                bool
	}{
		{"coderabbit", "owner/repo", 2, true},
		{"another-provider", "original-owner/original.repo", 2147483647, true},
		{"", "owner/repo", 2, false},
		{"provider\nInjected", "owner/repo", 2, false},
		{"coderabbit", "https://example.test/repo", 2, false},
		{"coderabbit", "owner/repo?redirect=elsewhere", 2, false},
		{"coderabbit", "../repo", 2, false},
		{"coderabbit", "owner/..", 2, false},
		{"coderabbit", "./repo", 2, false},
		{"coderabbit", "owner/repo", 0, false},
		{"coderabbit", "owner/repo", 2147483648, false},
	} {
		label, link, ok := completedReviewContext(tc.provider, tc.repository, tc.number)
		if ok != tc.valid || !ok && (label != "" || link != "") {
			t.Fatal(tc, label, link, ok)
		}
		if ok && (label != tc.provider && !(tc.provider == "coderabbit" && label == "CodeRabbit") || link != "https://github.com/"+tc.repository+"/pull/"+fmt.Sprint(tc.number)) {
			t.Fatal("context invented provider or link", tc, label, link)
		}
	}
}

func TestCompletionNoticeOldFormatRemainsImmutableAfterRestartAndReplay(t *testing.T) {
	f, a, evidence := completionNoticeFixture(t, "cli", true)
	if _, err := f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "completed", "fixture_completed", ""); err != nil {
		t.Fatal(err)
	}
	// Represent a notice produced by the predecessor binary, with the same
	// evidence-scoped key and atomic completed operation.
	oldBody := fmt.Sprintf("CLI review results ready for PR #2.\nEvidence: %s\nOperation: %s", evidence.ID, a.OperationID)
	admissionSQL(t, f, `UPDATE inbox_messages SET body=?`, oldBody)
	var sequence int
	var name, database string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &database); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(t.Context(), filepath.Dir(database))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	f.store = reopened
	f.e, err = New(reopened, attemptCapabilities, WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.RecoverRetainedCLI(t.Context(), "test-capability"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Enqueue(t.Context(), followingScope(t, f, "requester"), "explicit-review-request", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.PublishEvidence(t.Context(), a.ProjectID, evidence.ID, nil); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := f.store.DB().QueryRow(`SELECT body FROM inbox_messages`).Scan(&body); err != nil || body != oldBody || completionNoticeCount(t, f) != 1 {
		t.Fatal("restart or exact replay republished immutable old notice", body, err)
	}
}
