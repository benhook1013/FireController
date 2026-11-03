package reviewexec

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func followingFixture(t *testing.T) *attemptFixture {
	t.Helper()
	f := queueEnrollmentFixture(t)
	for _, worker := range []string{"participant", "other"} {
		admissionSQL(t, f, `INSERT INTO workers VALUES(?,?,?)`, attemptProject, worker, worker)
	}
	configureQueue(t, f, 0, 2, 4)
	return f
}
func followingScope(t *testing.T, f *attemptFixture, worker string) core.Scope {
	t.Helper()
	s := f.scope(t, attemptProject, f.writer, false)
	s.WorkerID = worker
	return s
}
func followingPreference(t *testing.T, f *attemptFixture, worker string, number int64) reviews.Following {
	t.Helper()
	v, err := reviews.ReadFollowing(t.Context(), f.store.DB(), attemptProject, worker, number)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func followingInput(revision int64, on bool) FollowingRequest {
	return FollowingRequest{ExpectedRevision: &revision, Following: &on}
}

func TestPRFollowingOwnWorkerCASReplayAndPersistentOptOut(t *testing.T) {
	f := followingFixture(t)
	scope := followingScope(t, f, "participant")
	raw, err := f.e.ConfigureFollowing(t.Context(), scope, "mute", 2, followingInput(0, false))
	if err != nil {
		t.Fatal(err)
	}
	v := followingPreference(t, f, "participant", 2)
	if v.Mode != "muted" || v.Revision != 1 {
		t.Fatal(v)
	}
	if _, err := f.e.Enqueue(t.Context(), scope, "actual-request", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)}); err != nil {
		t.Fatal(err)
	}
	if got := followingPreference(t, f, "participant", 2); got.Mode != "muted" || got.Revision != 1 {
		t.Fatal("explicit work overrode opt-out", got)
	}
	if _, err := f.e.ConfigureFollowing(t.Context(), scope, "stale", 2, followingInput(0, true)); err == nil {
		t.Fatal("stale preference accepted")
	}
	if _, err := f.e.ConfigureFollowing(t.Context(), scope, "follow", 2, followingInput(1, true)); err != nil {
		t.Fatal(err)
	}
	if replay, err := f.e.ConfigureFollowing(t.Context(), scope, "mute", 2, followingInput(0, false)); err != nil || string(replay) != string(raw) {
		t.Fatal("exact replay lost original result", err)
	}
	if got := followingPreference(t, f, "participant", 2); got.Mode != "follow" || got.Revision != 2 {
		t.Fatal(got)
	}
	if _, err := f.e.ConfigureFollowing(t.Context(), followingScope(t, f, "other"), "mute", 2, followingInput(0, false)); err == nil {
		t.Fatal("another worker recovered the first worker's receipt")
	}
	if got := followingPreference(t, f, "other", 2); got.Mode != "none" {
		t.Fatal("cross-worker mutation", got)
	}
	if _, err := f.e.ConfigureFollowing(t.Context(), scope, "untracked", 99, followingInput(0, true)); err == nil {
		t.Fatal("following silently tracked a missing PR")
	}
	if _, err := f.e.ConfigureFollowing(t.Context(), scope, "pre-mute", 99, followingInput(0, false)); err != nil {
		t.Fatal("could not mute future participation", err)
	}
}

func TestPRFollowingEnrolledQueueDoesNotWaitForSourceHydration(t *testing.T) {
	f := selectionFixture(t) // A valid enrolled queue exists before any provider metadata.
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_current_pulls`) != 0 {
		t.Fatal("fixture already hydrated")
	}
	scope := selectionScope(t, f, attemptProject, f.writer, "write")
	v, err := f.e.Following(t.Context(), scope, 2)
	if err != nil || !v.Tracked || v.Mode != "none" {
		t.Fatal("enrollment depended on hydration", v, err)
	}
	if _, err := f.e.ConfigureFollowing(t.Context(), scope, "follow-before-source", 2, followingInput(0, true)); err != nil {
		t.Fatal("freshly enrolled PR required another queue step", err)
	}
}

func TestPRFollowingHTTPStrictAuthorityAndReadDoesNotWrite(t *testing.T) {
	f := followingFixture(t)
	call := func(method, token, worker, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/v1/projects/"+attemptProject+"/reviews/pulls/2/following", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-FC-Worker", worker)
		r.Header.Set("Idempotency-Key", core.NewID("key"))
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w
	}
	before := attemptCount(t, f.store, `SELECT count(*) FROM receipts`)
	w := call("GET", f.reader, "participant", "")
	var v reviews.Following
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Mode != "none" || !v.Tracked {
		t.Fatal(w.Code, w.Body.String())
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM receipts`) != before {
		t.Fatal("read wrote a receipt")
	}
	if w := call("GET", f.reader, "", ""); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("PUT", f.reader, "participant", `{"expected_revision":0,"following":true}`); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, body := range []string{`{}`, `{"following":true}`, `{"expected_revision":0}`, `{"expected_revision":0,"following":null}`, `{"expected_revision":0,"following":true,"worker_id":"other"}`, `{"expected_revision":0,"following":true,"following":false}`} {
		if w := call("PUT", f.writer, "participant", body); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := call("PUT", f.writer, "participant", `{"expected_revision":0,"following":false}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestPRFollowingExplicitEnrollmentAndRequestAreAtomic(t *testing.T) {
	f := followingFixture(t)
	scope := followingScope(t, f, "participant")
	if _, err := f.e.ChangeQueue(t.Context(), scope, "enroll", "add", QueueChange{ExpectedConfigurationRevision: 1, PullNumber: 9}); err != nil {
		t.Fatal(err)
	}
	if followingPreference(t, f, "participant", 9).Mode != "auto" {
		t.Fatal("enrollment did not follow")
	}
	admissionSQL(t, f, `CREATE TRIGGER reject_follow_request BEFORE INSERT ON events WHEN NEW.event_type='review.requested' BEGIN SELECT RAISE(ABORT,'request rollback'); END`)
	if _, err := f.e.Enqueue(t.Context(), scope, "rolled-back", Request{ExpectedConfigurationRevision: 2, PullNumber: 4, Channel: "cli", HeadSHA: strings.Repeat("a", 40)}); err == nil {
		t.Fatal("synthetic failure accepted")
	}
	if followingPreference(t, f, "participant", 4).Mode != "none" {
		t.Fatal("failed request retained a follower")
	}
	admissionSQL(t, f, `DROP TRIGGER reject_follow_request`)
	if _, err := f.e.Enqueue(t.Context(), scope, "rolled-back", Request{ExpectedConfigurationRevision: 2, PullNumber: 4, Channel: "cli", HeadSHA: strings.Repeat("a", 40)}); err != nil {
		t.Fatal(err)
	}
	if followingPreference(t, f, "participant", 4).Mode != "auto" {
		t.Fatal("successful request did not follow")
	}
}

func TestPRFollowingAutomaticAuthorizerExcludedAndManualSeedRunsOnce(t *testing.T) {
	f := schedulingFixture(t)
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	var err error
	f.e, err = New(f.store, attemptCapabilities, WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	if followingPreference(t, f, "selection-worker", 2).Mode != "none" {
		t.Fatal("project scheduling authorizer became a PR follower")
	}
	// Model a real pre-feature manual request, without the new explicit-work hook.
	f.e.publishQueue = nil
	if _, err := f.e.Enqueue(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "old-manual", Request{ExpectedConfigurationRevision: 1, PullNumber: 4, Channel: "hosted", HeadSHA: strings.Repeat("a", 40)}); err != nil {
		t.Fatal(err)
	}
	if err := SeedNotificationFollowers(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if followingPreference(t, f, "selection-worker", 2).Mode != "none" || followingPreference(t, f, "selection-worker", 4).Mode != "auto" {
		t.Fatal("seed inferred automatic ownership or lost manual participation")
	}
	admissionSQL(t, f, `DELETE FROM review_pr_followers WHERE project_id=?`, attemptProject)
	if err := SeedNotificationFollowers(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if followingPreference(t, f, "selection-worker", 4).Mode != "none" {
		t.Fatal("seed repeated on restart")
	}
}

func TestPRFollowingAdjudicationFinalizationAndFixUseTheirActingWorkers(t *testing.T) {
	f, a, evidence := projectionFixture(t)
	for _, worker := range []string{"decider", "finalizer", "fixer"} {
		admissionSQL(t, f, `INSERT INTO workers VALUES(?,?,?)`, attemptProject, worker, worker)
	}
	adjudicationComplete(t, f, a)
	run := projectionRun(evidence)
	projectionPublish(t, f, reviews.NativePublication{Pull: projectionPull(2), Run: &run})
	_, err := f.e.DecideReviewFindings(t.Context(), followingScope(t, f, "decider"), "decisions", evidence.ID, FindingDecisionBatchRequest{ExpectedRevision: adjudicationRevision(0), Items: []FindingDecisionBatchItem{{Finding: adjudicationRevision(0), Disposition: "accepted", Reason: "fix"}, {Finding: adjudicationRevision(1), Disposition: "rejected", Reason: "not applicable"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.e.FinalizeReviewEvidence(t.Context(), followingScope(t, f, "finalizer"), "finalize", evidence.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.e.RecordFindingFix(t.Context(), followingScope(t, f, "fixer"), "fix", evidence.ID, FindingFixRequest{ExpectedRevision: adjudicationRevision(2), Finding: 0, ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("e", 40), ProofNote: "Focused proof passed."})
	if err != nil {
		t.Fatal(err)
	}
	for _, worker := range []string{"decider", "finalizer", "fixer"} {
		if followingPreference(t, f, worker, 2).Mode != "auto" {
			t.Fatal("missing acting participant", worker)
		}
	}
}

func TestPRFollowingVerifiedSelectionClosureNotifiesBeforeLifecycleAndDoesNotRepeat(t *testing.T) {
	for _, state := range []string{"closed", "merged"} {
		t.Run(state, func(t *testing.T) {
			f := selectionFixtureWithQueue(t, []int64{2})
			if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
				t.Fatal(err)
			}
			if err := inbox.EnsureSchema(t.Context(), f.store); err != nil {
				t.Fatal(err)
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
			baseline := projectionPull(2)
			baseline.MergeStateStatus = "CLEAN"
			seed := reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: 801, Source: "retained-source-baseline", ObservedAt: stamp(), Queue: []reviews.PullInput{baseline}}
			if w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "baseline", encoded(seed)); w.Code != 201 {
				t.Fatal(w.Code, w.Body.String())
			}
			scope := selectionScope(t, f, attemptProject, f.writer, "write")
			if _, err := f.e.ConfigureFollowing(t.Context(), scope, "follow", 2, followingInput(0, true)); err != nil {
				t.Fatal(err)
			}
			reader := &refreshTestReader{pull: func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
				p := lifecyclePull(number)
				p.State = "closed"
				p.Merged = state == "merged"
				p.Mergeability = "conflicting"
				return p, nil
			}}
			selectionEnqueue(t, f, "first-observer", "cli", nil)
			selectionNext(t, f, reader)
			var current reviews.PullResponse
			projectionRead(t, f, "/pulls/2", &current)
			var mergeState string
			if err := f.store.DB().QueryRow(`SELECT json_extract(p.metadata_json,'$.merge_state_status') FROM review_pulls p JOIN review_workspace w ON w.project_id=p.project_id AND w.snapshot_id=p.snapshot_id WHERE p.project_id=? AND p.number=2`, attemptProject).Scan(&mergeState); err != nil {
				t.Fatal(err)
			}
			if current.Pull.State != state || current.Pull.CIState != "success" || current.Pull.Mergeability != "mergeable" || mergeState != "CLEAN" {
				t.Fatal("selection lost closure or invented CI/merge checks", current.Pull)
			}
			if n := attemptCount(t, f.store, `SELECT count(*) FROM inbox_messages WHERE origin_kind='controller' AND recipient_worker_id='selection-worker'`); n != 1 {
				t.Fatal("verified selection consumed transition without exactly one notice", n)
			}
			var actor string
			if err := f.store.DB().QueryRow(`SELECT actor_id FROM events WHERE event_type='message.sent'`).Scan(&actor); err != nil || actor != "exec-writer-principal" {
				t.Fatal("selection observer provenance changed", actor, err)
			}
			selectionEnqueue(t, f, "repeat-observer", "cli", nil)
			selectionNext(t, f, reader)
			options, _ := lifecycleOptions(nil)
			lifecycleTick(t, f, newLifecycleSchedule(options), reader)
			if n := attemptCount(t, f.store, `SELECT count(*) FROM inbox_messages`); n != 1 {
				t.Fatal("repeated selection/lifecycle observation duplicated notice", n)
			}
		})
	}
}

func TestPRFollowingSeedRejectsMalformedMarkerWithoutChangingPreferences(t *testing.T) {
	f := followingFixture(t)
	admissionSQL(t, f, `INSERT INTO metadata(key,value) VALUES('review_notifications:manual_seed_v1','0')`)
	if err := SeedNotificationFollowers(t.Context(), f.store); err == nil {
		t.Fatal("malformed marker silently suppressed participant seed")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_pr_followers`) != 0 {
		t.Fatal("invalid marker changed preferences")
	}
}
