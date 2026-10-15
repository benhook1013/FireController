package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

type readinessTestReader struct {
	sourceCITestReader
	review      func(context.Context, reviewprovider.CITarget) (reviewprovider.PullReviewObservation, error)
	reviewCalls atomic.Int64
}

func (r *readinessTestReader) ObservePullReviewState(ctx context.Context, target reviewprovider.CITarget) (reviewprovider.PullReviewObservation, error) {
	r.reviewCalls.Add(1)
	if r.review != nil {
		return r.review(ctx, target)
	}
	return reviewprovider.PullReviewObservation{Target: target, State: "complete", ObservedAt: time.Now().UTC(), PullState: "OPEN", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", Threads: []reviewprovider.PullReviewThread{}}, nil
}
func newReadinessTestReader() *readinessTestReader {
	r := &readinessTestReader{}
	r.ci = func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		o := sourceCITestObservation(target, "not_required")
		o.Candidate.MergeState = "verified"
		o.Candidate.MergeSHA = strings.Repeat("c", 40)
		o.MergeState = "verified"
		return o, nil
	}
	return r
}
func readinessRequest(number, revision int64) SourceRefreshRequest {
	r := refreshRequest()
	r.PullNumber = number
	r.ExpectedConfigurationRevision = revision
	r.IncludeReadiness = true
	return r
}
func collectReadiness(t *testing.T, f *attemptFixture, r *readinessTestReader, key string, number, revision int64) SourceRefreshOperation {
	t.Helper()
	queued := refreshEnqueue(t, f, key, readinessRequest(number, revision))
	cancel, done := refreshStart(t, f, r)
	got := refreshWait(t, f, queued.ID, "completed")
	refreshStop(t, cancel, done)
	return got
}
func readinessFixture(t *testing.T) (*attemptFixture, Attempt, string, *readinessTestReader, SourceRefreshOperation) {
	t.Helper()
	f, a, id, _ := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	assessmentPolicy(t, f, 1, []string{"cli"}, []string{})
	r := newReadinessTestReader()
	got := collectReadiness(t, f, r, "readiness", 2, 1)
	return f, a, id, r, got
}
func readinessDurable(t *testing.T, f *attemptFixture, a Attempt) string {
	t.Helper()
	var rows string
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(id||revision||state||observation_json||ci_observation_json||review_state_json,''),'') FROM review_source_refreshes`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return adjudicationState(t, f, a) + policyState(t, f) + rows
}
func readReadiness(t *testing.T, f *attemptFixture, r *readinessTestReader, a Attempt, number int64) SourceReadinessAssessment {
	t.Helper()
	before, calls, ciCalls, reviewCalls := readinessDurable(t, f, a), r.calls.Load(), r.ciCalls.Load(), r.reviewCalls.Load()
	got, err := f.e.ReadSourceReadiness(t.Context(), sourceCIScope(t, f), number)
	if err != nil {
		t.Fatal(err)
	}
	if before != readinessDurable(t, f, a) || calls != r.calls.Load() || ciCalls != r.ciCalls.Load() || reviewCalls != r.reviewCalls.Load() {
		t.Fatal("local readiness read mutated state or called provider")
	}
	return got
}
func TestSourceReadinessReadyCompactReadAndOptionsReplay(t *testing.T) {
	f, a, _, r, op := readinessFixture(t)
	if op.IncludeCI || !op.IncludeReadiness || op.CI == nil || op.ReviewState == nil || op.CI.State != "not_required" {
		t.Fatal("request options rewritten or collections missing", op)
	}
	got := readReadiness(t, f, r, a, 2)
	if got.State != "ready_as_observed" || len(got.Reasons) != 0 || got.Source != "observed_comparison" || got.PolicyRevision != 2 || len(got.Channels) != 1 || !got.Channels[0].Covered || got.ReviewState.TotalThreads != 0 || got.ReviewState.ReviewDecision != nil {
		t.Fatal(got)
	}
	body, _ := json.Marshal(got)
	if len(body) > 4096 || strings.Contains(string(body), "threads\":[") || strings.Contains(string(body), "authority") {
		t.Fatal("readiness leaked full inventory or grew unbounded", string(body))
	}
	before := readinessDurable(t, f, a)
	replay := refreshEnqueue(t, f, "readiness", readinessRequest(2, 1))
	if replay.ID != op.ID || before != readinessDurable(t, f, a) {
		t.Fatal("terminal exact replay changed observation")
	}
	ci, err := f.e.ReadSourceCIAssessment(t.Context(), sourceCIScope(t, f), 2)
	if err != nil || ci.RefreshID != op.ID || ci.State != "not_required" {
		t.Fatal("readiness collection unavailable to explicit CI read", ci, err)
	}
	// Execution pauses preserve already earned review credit and source binding.
	f.configure(t, attemptProject, "test-capability", 1, true)
	assessmentPolicy(t, f, 2, []string{"cli"}, []string{"cli"})
	if paused := readReadiness(t, f, r, a, 2); paused.State != "ready_as_observed" || paused.ConfigurationRevision != 2 || paused.PolicyRevision != 3 {
		t.Fatal(paused)
	}
}
func TestSourceReadinessOptionsCoalesceOnlyExactRequestedFacts(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	plain := refreshEnqueue(t, f, "plain", refreshRequest())
	ci := refreshEnqueue(t, f, "ci", sourceCIRequest())
	ready := refreshEnqueue(t, f, "ready", readinessRequest(2, 1))
	bothReq := readinessRequest(2, 1)
	bothReq.IncludeCI = true
	both := refreshEnqueue(t, f, "both", bothReq)
	if len(map[string]bool{plain.ID: true, ci.ID: true, ready.ID: true, both.ID: true}) != 4 {
		t.Fatal("distinct option identities coalesced")
	}
	if other := refreshEnqueue(t, f, "another-ready", readinessRequest(2, 1)); other != ready {
		t.Fatal("same requested facts did not coalesce")
	}
	_, err := f.e.EnqueueSourceRefresh(t.Context(), f.scope(t, attemptProject, f.writer, false), "plain", readinessRequest(2, 1))
	attemptCode(t, err, "idempotency_conflict")
	r := newReadinessTestReader()
	cancel, done := refreshStart(t, f, r)
	refreshWait(t, f, both.ID, "completed")
	refreshStop(t, cancel, done)
	if r.calls.Load() != 8 || r.ciCalls.Load() != 3 || r.reviewCalls.Load() != 2 {
		t.Fatal("default modes broadened provider work", r.calls.Load(), r.ciCalls.Load(), r.reviewCalls.Load())
	}
}
func TestSourceReadinessUnknownAndBlockingProviderFacts(t *testing.T) {
	for _, name := range []string{"current-thread", "outdated-thread", "draft", "closed", "conflict", "blocked", "changes-requested", "review-required", "unknown-review", "ci-failed", "ci-unavailable", "review-error", "malformed-provider"} {
		t.Run(name, func(t *testing.T) {
			f, a, _, r, _ := readinessFixture(t)
			reason := map[string]string{"current-thread": "unresolved_current_threads", "outdated-thread": "unresolved_outdated_threads", "draft": "pull_draft", "closed": "pull_not_open", "conflict": "mergeability_unproven", "blocked": "merge_state_not_clean", "changes-requested": "review_changes_requested", "review-required": "review_required", "unknown-review": "review_state_unproven", "ci-failed": "ci_failed", "ci-unavailable": "ci_unproven", "review-error": "review_state_unproven", "malformed-provider": "review_state_unproven"}[name]
			r.review = func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.PullReviewObservation, error) {
				o := reviewprovider.PullReviewObservation{Target: target, State: "complete", ObservedAt: time.Now().UTC(), PullState: "OPEN", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", Threads: []reviewprovider.PullReviewThread{}}
				switch name {
				case "current-thread":
					o.Threads = []reviewprovider.PullReviewThread{{ID: "thread_current"}}
				case "outdated-thread":
					o.Threads = []reviewprovider.PullReviewThread{{ID: "thread_outdated", Outdated: true}}
				case "draft":
					o.Draft = true
				case "closed":
					o.PullState = "CLOSED"
				case "conflict":
					o.Mergeable = "CONFLICTING"
				case "blocked":
					o.MergeStateStatus = "BLOCKED"
				case "changes-requested":
					v := "CHANGES_REQUESTED"
					o.ReviewDecision = &v
				case "review-required":
					v := "REVIEW_REQUIRED"
					o.ReviewDecision = &v
				case "unknown-review":
					return *unavailableSourceReview(target, "review_state_changed"), nil
				case "review-error":
					return o, errors.New("private transport diagnostics")
				case "malformed-provider":
					o.Threads = nil
				}
				return o, nil
			}
			if name == "ci-failed" {
				r.ci = func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
					return sourceCITestObservation(target, "failed"), nil
				}
			}
			if name == "ci-unavailable" {
				r.ci = func(context.Context, reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
					return reviewprovider.CIObservation{}, errors.New("private credential")
				}
			}
			collectReadiness(t, f, r, "next-readiness", 2, 1)
			got := readReadiness(t, f, r, a, 2)
			if got.State != "held" || !slices.Contains(got.Reasons, reason) {
				t.Fatal(name, got)
			}
			if strings.Contains(encoded(got), "private") {
				t.Fatal("raw provider diagnostic exposed")
			}
		})
	}
}
func TestSourceReadinessCurrentLocalApplicabilityAndMissingPolicy(t *testing.T) {
	for _, change := range []string{"unconfigured", "configuration", "repository", "capability", "head", "base"} {
		t.Run(change, func(t *testing.T) {
			f, a, _, r, _ := readinessFixture(t)
			want := "source_configuration_changed"
			switch change {
			case "unconfigured":
				admissionSQL(t, f, `DELETE FROM review_policy_config WHERE project_id=?`, attemptProject)
				want = "policy_unconfigured"
			case "configuration":
				f.configure(t, attemptProject, "other-account", 1, false)
			case "repository":
				admissionSQL(t, f, `UPDATE projects SET github_repository_id=999 WHERE id=?`, attemptProject)
			case "capability":
				var err error
				caps := append([]Capability{}, attemptCapabilities...)
				caps[0].Fingerprint = strings.Repeat("9", 64)
				f.e, err = New(f.store, caps)
				if err != nil {
					t.Fatal(err)
				}
			case "head", "base":
				r.pull = func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
					p := reviewprovider.Pull{Number: n, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}
					if change == "head" {
						p.HeadSHA = strings.Repeat("d", 40)
					} else {
						p.BaseSHA = strings.Repeat("e", 40)
					}
					return p, nil
				}
				request := refreshRequest()
				if change == "head" {
					request.HeadSHA = strings.Repeat("d", 40)
				}
				queued := refreshEnqueue(t, f, "later-source-only", request)
				cancel, done := refreshStart(t, f, r)
				refreshWait(t, f, queued.ID, "completed")
				refreshStop(t, cancel, done)
				want = "source_comparison_changed"
			}
			got := readReadiness(t, f, r, a, 2)
			if !reflect.DeepEqual(got.Reasons, []string{want}) || change == "unconfigured" && got.State != "unconfigured" || change != "unconfigured" && got.State != "held" {
				t.Fatal(got)
			}
		})
	}
}
func TestSourceReadinessPrivateCorruptionAndPublicAuthorization(t *testing.T) {
	f, a, _, r, op := readinessFixture(t)
	var original string
	if err := f.store.DB().QueryRow(`SELECT review_state_json FROM review_source_refreshes WHERE id=?`, op.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"null", "{}", strings.Replace(original, `"threads":[]`, `"threads":null`, 1), strings.Replace(original, `"draft":false`, `"draft":null`, 1), strings.Replace(original, `"state":"complete"`, `"state":"complete","unknown":1`, 1), strings.Replace(original, `"head_sha":"`+strings.Repeat("a", 40), `"head_sha":"`+strings.Repeat("b", 40), 1)} {
		admissionSQL(t, f, `UPDATE review_source_refreshes SET review_state_json=? WHERE id=?`, body, op.ID)
		before := readinessDurable(t, f, a)
		_, err := f.e.ReadSourceReadiness(t.Context(), sourceCIScope(t, f), 2)
		attemptCode(t, err, "storage_unavailable")
		if before != readinessDurable(t, f, a) {
			t.Fatal("corrupt read changed state")
		}
	}
	admissionSQL(t, f, `UPDATE review_source_refreshes SET review_state_json=? WHERE id=?`, original, op.ID)
	path := "/api/v1/projects/" + attemptProject + "/reviews/source/2/readiness"
	if w := f.request(http.MethodGet, path, f.reader, "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "ready_as_observed") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, bad := range []string{path + "?refresh=true", strings.Replace(path, "/2/", "/02/", 1)} {
		if w := f.request(http.MethodGet, bad, f.reader, "", ""); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := f.request(http.MethodGet, path, f.other, "", ""); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	scope := sourceCIScope(t, f)
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-reader-cred'`)
	_, err := f.e.ReadSourceReadiness(t.Context(), scope, 2)
	attemptCode(t, err, "unauthorized")
	_ = r
}
func TestSourceReadinessRestartRetainsFactsAndFailedExtraPreservesSource(t *testing.T) {
	f, a, _, r, op := readinessFixture(t)
	var database string
	rows, err := f.store.DB().Query(`PRAGMA database_list`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var sequence int
		var name string
		if err = rows.Scan(&sequence, &name, &database); err != nil {
			t.Fatal(err)
		}
		if name == "main" {
			break
		}
	}
	rows.Close()
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(t.Context(), filepath.Dir(database))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	f.store = reopened
	f.e, err = New(reopened, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	got := refreshRead(t, f, op.ID)
	if !reflect.DeepEqual(got.ReviewState, op.ReviewState) || got.CI == nil {
		t.Fatal("restart lost typed facts", got)
	}
	if readReadiness(t, f, r, a, 2).State != "ready_as_observed" {
		t.Fatal("restart changed verdict")
	}
	r.review = func(context.Context, reviewprovider.CITarget) (reviewprovider.PullReviewObservation, error) {
		return reviewprovider.PullReviewObservation{}, errors.New("offline")
	}
	failed := collectReadiness(t, f, r, "failed-extra", 2, 1)
	if failed.State != "completed" || failed.Observation == nil || failed.ReviewState.State != "unproven" {
		t.Fatal("extra failure discarded useful source", failed)
	}
	if got := readReadiness(t, f, r, a, 2); got.State != "held" {
		t.Fatal(got)
	}
}

func TestSourceReadinessIncomingObligationsHoldAlreadyCoveredReceiver(t *testing.T) {
	f, source, sourceID := routeFixture(t, 1)
	adjudicationComplete(t, f, source)
	receiver, _, _ := routePolicyReceiver(t, f, sourceID)
	reader := newReadinessTestReader()
	collectReadiness(t, f, reader, "receiver-readiness", 4, 2)
	if got := readReadiness(t, f, reader, receiver, 4); got.State != "ready_as_observed" {
		t.Fatal(got)
	}
	route := routeCreate(t, f, sourceID, "incoming-readiness-route", routeRequest(0, 0, 4))
	raw, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "readiness-route-finalize", sourceID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
	adjudicationReceipt(t, raw, err, sourceID, "finalized", 2)
	original := adjudicationRead(t, f, sourceID)
	for _, step := range []string{"undecided", "accepted", "fixed"} {
		if step == "accepted" {
			routeDecision(t, f, route.ID, "readiness-route-accept", 1, "accepted")
		}
		if step == "fixed" {
			routeFix(t, f, route.ID, "readiness-route-fix", 2, "", strings.Repeat("f", 40))
		}
		got := readReadiness(t, f, reader, receiver, 4)
		if len(got.Channels) != 1 || !got.Channels[0].Covered || got.Channels[0].Pending {
			t.Fatal("incoming obligation replaced actual channel credit", got)
		}
		if step == "fixed" {
			if got.State != "ready_as_observed" || got.IncomingRoutesPending != 0 {
				t.Fatal(got)
			}
		} else if got.State != "held" || got.IncomingRoutesPending != 1 || !slices.Contains(got.Reasons, "routed_findings_pending") {
			t.Fatal(got)
		}
	}
	if !reflect.DeepEqual(original, adjudicationRead(t, f, sourceID)) {
		t.Fatal("readiness or receiver proof rewrote source")
	}
}

func TestSourceReadinessPendingNativeCaptureAndRequiredPolicyChange(t *testing.T) {
	f, a, id, _ := coverageFixture(t, 1, false)
	assessmentPolicy(t, f, 1, []string{"cli"}, []string{})
	adjudicationComplete(t, f, a)
	r := newReadinessTestReader()
	collectReadiness(t, f, r, "pending-native-readiness", 2, 1)
	got := readReadiness(t, f, r, a, 2)
	if got.State != "held" || !slices.Contains(got.Reasons, "review_evidence_pending") {
		t.Fatal(got)
	}
	adjudicationDecide(t, f, id, "readiness-reject-native", 0, 0, "rejected")
	raw, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "readiness-finalize-native", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
	adjudicationReceipt(t, raw, err, id, "finalized", 2)
	if got = readReadiness(t, f, r, a, 2); got.State != "ready_as_observed" {
		t.Fatal(got)
	}
	assessmentPolicy(t, f, 2, []string{"cli", "hosted"}, []string{})
	if got = readReadiness(t, f, r, a, 2); got.State != "held" || !slices.Contains(got.Reasons, "review_coverage_missing") || got.PolicyRevision != 3 {
		t.Fatal(got)
	}
}

func TestSourceReadinessProviderWaitCancellationAndRecoverySameOperation(t *testing.T) {
	f, a, _, reader, _ := readinessFixture(t)
	entered := make(chan struct{})
	reader.review = func(ctx context.Context, _ reviewprovider.CITarget) (reviewprovider.PullReviewObservation, error) {
		close(entered)
		<-ctx.Done()
		return reviewprovider.PullReviewObservation{}, ctx.Err()
	}
	receipt := refreshEnqueue(t, f, "interrupted-readiness", readinessRequest(2, 1))
	cancel, done := refreshStart(t, f, reader)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("review-state reader not reached")
	}
	ctx, stop := context.WithTimeout(t.Context(), time.Second)
	err := f.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE projects SET name='Coordination write during readiness' WHERE id=?`, attemptProject)
		return err
	})
	stop()
	if err != nil {
		t.Fatal("provider wait held writer", err)
	}
	refreshStop(t, cancel, done)
	if got := refreshRead(t, f, receipt.ID); got.State != "running" || got.ReviewState != nil || got.CI != nil {
		t.Fatal("cancel fabricated terminal result", got)
	}
	var database string
	if err = f.store.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = core.Open(t.Context(), filepath.Dir(database))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	if err = EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	f.e, err = New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	fresh := newReadinessTestReader()
	cancel, done = refreshStart(t, f, fresh)
	completed := refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	if !completed.IncludeReadiness || completed.ID != receipt.ID || readReadiness(t, f, fresh, a, 2).State != "ready_as_observed" || fresh.reviewCalls.Load() != 1 {
		t.Fatal("restart did not recover same bounded read", completed)
	}
}

func TestSourceReadinessStrictHTTPOptionAndMissingAssessment(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	get := "/api/v1/projects/" + attemptProject + "/reviews/source/2/readiness"
	if w := f.request(http.MethodGet, get, f.reader, "", ""); w.Code != 409 || !strings.Contains(w.Body.String(), "reviews_readiness_unavailable") {
		t.Fatal(w.Code, w.Body.String())
	}
	post := "/api/v1/projects/" + attemptProject + "/reviews/source-refreshes"
	zero := "wait=0"
	valid := encoded(readinessRequest(2, 1))
	for _, body := range []string{strings.Replace(valid, `"include_readiness":true`, `"include_readiness":null`, 1), strings.Replace(valid, `"include_readiness":true`, `"include_readiness":"true"`, 1), strings.Replace(valid, `"include_readiness":true`, `"include_readiness":true,"include_readiness":false`, 1), strings.Replace(valid, `"include_readiness":true`, `"Include_readiness":true`, 1)} {
		before := attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes`)
		w := sourceHTTPRequest(f, http.MethodPost, post, f.writer, "invalid-readiness-option", body, &zero)
		if w.Code != 400 || before != attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := sourceHTTPRequest(f, http.MethodPost, post, f.writer, "valid-readiness-option", valid, &zero)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestSourceReadinessCIMergeFactCannotBeOverriddenBySeparateCleanReviewState(t *testing.T) {
	for _, merge := range []string{"conflicting", "pending", "unproven", ""} {
		t.Run(merge, func(t *testing.T) {
			f, a, _, reader, _ := readinessFixture(t)
			reader.ci = func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				if merge == "pending" || merge == "unproven" || merge == "" {
					o := unavailableSourceCI(target, time.Now().UTC(), "ci_observation_unproven")
					o.MergeState = merge
					return *o, nil
				}
				return sourceCITestObservation(target, "not_required"), nil
			}
			collectReadiness(t, f, reader, "different-ci-merge", 2, 1)
			got := readReadiness(t, f, reader, a, 2)
			if got.State != "held" || !slices.Contains(got.Reasons, "ci_merge_state_unproven") || got.ReviewState.Mergeable != "MERGEABLE" || got.ReviewState.MergeStateStatus != "CLEAN" {
				t.Fatal(got)
			}
		})
	}
}

func TestSourceReadinessIncomingCapacityIsExplicitHold(t *testing.T) {
	f, source, sourceID := routeFixture(t, 100)
	adjudicationComplete(t, f, source)
	receiver, _, _ := routePolicyReceiver(t, f, sourceID)
	reader := newReadinessTestReader()
	collectReadiness(t, f, reader, "receiver-capacity-readiness", 4, 2)
	for i := range 100 {
		routeCreate(t, f, sourceID, fmt.Sprintf("readiness-capacity-%d", i), routeRequest(int64(i), int64(i), 4))
	}
	next := f.start(t, attemptProject, f.enqueue(t, attemptProject, "readiness-extra-source", "cli", 6, 2))
	anchor := attemptAnchor(attemptProject, 6)
	var err error
	next, err = f.e.Admit(t.Context(), attemptProject, next.ID, next.Revision, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	original, err := f.e.ReadReviewEvidence(t.Context(), sourceCIScope(t, f), sourceID)
	if err != nil {
		t.Fatal(err)
	}
	capture := evidenceClone(original.Capture)
	capture.ExpectedAnchor = anchor
	capture.CaptureSHA256 = strings.Repeat("9", 64)
	capture.CaptureID = "review_capture_" + capture.CaptureSHA256[:32]
	capture.Review.Findings = capture.Review.Findings[:1]
	capture.Review.ReportedFindings = 1
	evidence, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, next.ID, capture)
	if err != nil {
		t.Fatal(err)
	}
	routeCreate(t, f, evidence.ID, "readiness-capacity-101", routeRequest(0, 0, 4))
	got := readReadiness(t, f, reader, receiver, 4)
	if got.State != "held" || !got.IncomingRoutesCapacity || got.IncomingRoutesPending != 0 || !slices.Contains(got.Reasons, "routed_findings_capacity") || !got.Channels[0].Covered {
		t.Fatal(got)
	}
}

func TestSourceReadinessCollectionUsesOneBoundedBudget(t *testing.T) {
	f, a, _, reader, _ := readinessFixture(t)
	reader.ci = func(ctx context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		deadline, ok := ctx.Deadline()
		remaining := time.Until(deadline)
		if !ok || remaining <= 30*time.Second || remaining > 45*time.Second {
			t.Error("readiness does not share selected45s budget", remaining)
		}
		o := sourceCITestObservation(target, "not_required")
		o.Candidate.MergeState = "verified"
		o.Candidate.MergeSHA = strings.Repeat("c", 40)
		o.MergeState = "verified"
		return o, nil
	}
	reader.review = func(ctx context.Context, target reviewprovider.CITarget) (reviewprovider.PullReviewObservation, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 45*time.Second {
			t.Error("review collection escaped operation budget")
		}
		return reviewprovider.PullReviewObservation{Target: target, State: "complete", ObservedAt: time.Now().UTC(), PullState: "OPEN", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", Threads: []reviewprovider.PullReviewThread{}}, nil
	}
	collectReadiness(t, f, reader, "bounded-readiness", 2, 1)
	if readReadiness(t, f, reader, a, 2).State != "ready_as_observed" {
		t.Fatal("bounded collection lost facts")
	}
}

func TestSourceReadinessImportedBrowserHistoryCannotSupplyNativeCredit(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	assessmentPolicy(t, f, 1, []string{"cli"}, []string{})
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, f.store)
	f.handler = mux
	pull := projectionPull(2)
	pull.Runs = []reviews.RunInput{{SourceRunID: "historical-zero-review", Channel: "cli", HeadSHA: strings.Repeat("a", 40), Reviewer: "Historical reviewer", Scope: "Retained historical comparison", Outcome: "completed", Attributable: true, Complete: true, StartedAt: stamp(), FinishedAt: stamp(), Finalized: true, CoverageLimits: []string{}, Findings: []reviews.Finding{}}}
	request := reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: 801, Source: "historical-import", ObservedAt: stamp(), Queue: []reviews.PullInput{pull}, Retired: []reviews.RetiredPull{}}
	response := f.request(http.MethodPost, "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "readiness-history", encoded(request))
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	reader := newReadinessTestReader()
	collectReadiness(t, f, reader, "imported-readiness", 2, 1)
	before, calls := sourceCITestState(t, f), reader.reviewCalls.Load()
	got, err := f.e.ReadSourceReadiness(t.Context(), sourceCIScope(t, f), 2)
	if err != nil || got.State != "held" || !slices.Contains(got.Reasons, "review_coverage_missing") || len(got.Channels) != 1 || got.Channels[0].Covered || got.Channels[0].Pending {
		t.Fatal("imported zero-review supplied native credit", got, err)
	}
	if before != sourceCITestState(t, f) || calls != reader.reviewCalls.Load() || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
		t.Fatal("read invented evidence/provider work")
	}
}
