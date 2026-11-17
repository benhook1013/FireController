package reviewexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func queueEnrollmentFixture(t *testing.T) *attemptFixture {
	t.Helper()
	f := newAttemptFixture(t)
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
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
	return f
}
func configuredQueue(t *testing.T, f *attemptFixture) reviews.QueueResponse {
	t.Helper()
	var q reviews.QueueResponse
	projectionRead(t, f, "/queue?limit=100", &q)
	return q
}
func queueNumbers(q reviews.QueueResponse) []int64 {
	out := []int64{}
	for _, p := range q.Items {
		out = append(out, p.Number)
	}
	return out
}
func configureQueue(t *testing.T, f *attemptFixture, rev int64, numbers ...int64) {
	t.Helper()
	_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), fmt.Sprintf("queue-config-%d", rev), enrollmentConfiguration(rev, numbers, []string{"cli", "hosted"}))
	if err != nil {
		t.Fatal(err)
	}
}
func TestExecutionQueueEnrollmentIsVisibleAtomicAndProviderFree(t *testing.T) {
	f := queueEnrollmentFixture(t)
	c := enrollmentConfiguration(0, []int64{9, 2}, []string{"cli", "hosted"})
	scope := f.scope(t, attemptProject, f.admin, true)
	first, err := f.e.Configure(t.Context(), scope, "single-enrollment", c)
	if err != nil {
		t.Fatal(err)
	}
	q := configuredQueue(t, f)
	if !reflect.DeepEqual(queueNumbers(q), c.Queue) || q.Revision != 1 {
		t.Fatal(q)
	}
	for _, p := range q.Items {
		if p.Title != fmt.Sprintf("PR #%d", p.Number) || p.State != "unknown" || p.HeadSHA != "" || p.CIState != "unknown" || p.Mergeability != "unknown" || p.Complete || p.RunCount != 0 {
			t.Fatal("invented source/review facts", p)
		}
		for _, channel := range c.Channels {
			assertEnrollmentDefault(t, f, p.Number, channel)
		}
	}
	before := projectionDigest(t, f, "") + enrollmentState(t, f)
	replay, err := f.e.Configure(t.Context(), scope, "single-enrollment", c)
	if err != nil || !bytes.Equal(first, replay) || before != projectionDigest(t, f, "")+enrollmentState(t, f) {
		t.Fatal("replay changed queue/controls", err)
	}
	for _, table := range []string{"review_operations", "review_execution_attempts", "review_execution_evidence", "review_round_completions"} {
		if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("configuration started review work", table)
		}
	}
	// Other project has independent membership and order.
	_, err = f.e.Configure(t.Context(), f.scope(t, otherAttemptProject, f.admin, true), "other-queue", enrollmentConfiguration(0, []int64{4}, []string{"cli"}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(queueNumbers(configuredQueue(t, f)), c.Queue) {
		t.Fatal("cross-project queue changed")
	}
}
func TestExecutionQueueInsertionReorderPreservesFactsHistoryAndControls(t *testing.T) {
	f := queueEnrollmentFixture(t)
	// Preserve an unrelated merged entry and its original evidence.
	p := projectionPull(1)
	p.State = "merged"
	p.Runs = []reviews.RunInput{{SourceRunID: "original", Channel: "cli", HeadSHA: p.HeadSHA, Reviewer: "Independent", Scope: "patch", Outcome: "completed", StartedAt: "2026-01-01T01:00:00Z", FinishedAt: "2026-01-01T01:02:00Z", Attributable: true, Complete: true, Finalized: true, Findings: []reviews.Finding{}}}
	req := reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: 801, Source: "original", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{p, projectionPull(9), projectionPull(2)}}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "original", encoded(req))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var old reviews.RunDetailResponse
	var runs reviews.RunsResponse
	projectionRead(t, f, "/pulls/1/runs", &runs)
	oldBody := projectionRead(t, f, "/runs/"+runs.Items[0].ID, &old)
	configureQueue(t, f, 0, 9, 2)
	initial := configuredQueue(t, f)
	if initial.Revision != 1 {
		t.Fatal("unchanged enrollment published unnecessary snapshot", initial)
	}
	controls := roundRead(t, f, 9, "cli")
	configureQueue(t, f, 1, 9, 8, 2)
	if got := queueNumbers(configuredQueue(t, f)); !reflect.DeepEqual(got, []int64{1, 9, 8, 2}) {
		t.Fatal(got)
	}
	configureQueue(t, f, 2, 2, 9, 8)
	got := configuredQueue(t, f)
	if !reflect.DeepEqual(queueNumbers(got), []int64{1, 2, 9, 8}) {
		t.Fatal(got)
	}
	if got.Items[2].Title != projectionPull(9).Title || !got.Items[2].Complete || got.Items[0].State != "merged" {
		t.Fatal("source metadata altered", got)
	}
	var detail reviews.RunDetailResponse
	if !bytes.Equal(oldBody, projectionRead(t, f, "/runs/"+runs.Items[0].ID+"?snapshot="+old.SnapshotID, &detail)) {
		t.Fatal("historical review mutated")
	}
	if !reflect.DeepEqual(roundRead(t, f, 9, "cli"), controls) {
		t.Fatal("reorder changed existing allowance")
	}
	// Removing scheduling does not delete completed history or silently retire work.
	configureQueue(t, f, 3, 2)
	if !reflect.DeepEqual(queueNumbers(configuredQueue(t, f)), []int64{1, 2, 9, 8}) {
		t.Fatal("removed retained history")
	}
	before := projectionDigest(t, f, "")
	c := enrollmentConfiguration(4, []int64{2}, []string{"cli", "hosted"})
	c.Paused = true
	c.Mode = "stacked"
	c.CapabilityID = "other-account"
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "pause-only", c); err != nil {
		t.Fatal(err)
	}
	if projectionDigest(t, f, "") != before {
		t.Fatal("pause/mode/capability generated queue churn")
	}
}
func TestExecutionQueueEnrollmentRollsBackWithEitherEventFailure(t *testing.T) {
	for _, event := range []string{"review.observed", "reviews.configured"} {
		t.Run(event, func(t *testing.T) {
			f := queueEnrollmentFixture(t)
			before := projectionDigest(t, f, "") + enrollmentState(t, f)
			admissionSQL(t, f, `CREATE TRIGGER fail_queue_event BEFORE INSERT ON events WHEN NEW.event_type='`+event+`' BEGIN SELECT RAISE(ABORT,'queue test failure'); END`)
			c := enrollmentConfiguration(0, []int64{2, 4}, []string{"cli", "hosted"})
			_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "atomic-config", c)
			if err == nil || before != projectionDigest(t, f, "")+enrollmentState(t, f) {
				t.Fatal("partial queue/config/allowance commit", err)
			}
			admissionSQL(t, f, `DROP TRIGGER fail_queue_event`)
			if _, err = f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "atomic-config", c); err != nil {
				t.Fatal("same-key retry after rollback", err)
			}
			if !reflect.DeepEqual(queueNumbers(configuredQueue(t, f)), c.Queue) {
				t.Fatal("retry omitted selection")
			}
		})
	}
}
func TestExecutionQueueEnrollmentPreservesExplicitRetirement(t *testing.T) {
	f := queueEnrollmentFixture(t)
	configureQueue(t, f, 0, 2)
	q := configuredQueue(t, f)
	req := reviews.QueueEditRequest{ExpectedRevision: &q.Revision, RepositoryID: 801, Source: "operator-retirement", Queue: []reviews.QueuePullInput{}, Retired: []reviews.RetiredPull{{Number: 2, Reason: "superseded"}}}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/queue/edits", f.admin, "retire", encoded(req))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	configureQueue(t, f, 1, 2, 4)
	if !reflect.DeepEqual(queueNumbers(configuredQueue(t, f)), []int64{4}) {
		t.Fatal("reactivated retired predecessor")
	}
	configureQueue(t, f, 2, 4)
	before := projectionDigest(t, f, "") + enrollmentState(t, f)
	_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "reject-retired-enrollment", enrollmentConfiguration(3, []int64{4, 2}, []string{"cli", "hosted"}))
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Code != "review_target_retired" || before != projectionDigest(t, f, "")+enrollmentState(t, f) {
		t.Fatal("retired PR silently reactivated", err)
	}
}
func TestExecutionQueuePromotesReviewOnlyEntryWithoutDuplicatingEvidence(t *testing.T) {
	f := queueEnrollmentFixture(t)
	in := reviews.SubagentRecordRequest{PullNumber: 7, HeadSHA: strings.Repeat("a", 40), Reviewer: "Independent", Scope: "source", StartedAt: "2026-01-01T01:00:00Z", FinishedAt: "2026-01-01T01:02:00Z", Finalized: true, Findings: []reviews.SubagentFindingInput{}}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/subagent-reports", f.writer, "actual-subagent", encoded(in))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var receipt reviews.SubagentReceipt
	if json.Unmarshal(w.Body.Bytes(), &receipt) != nil {
		t.Fatal(w.Body.String())
	}
	if len(configuredQueue(t, f).Items) != 0 {
		t.Fatal("subagent result implicitly enrolled")
	}
	var old reviews.RunDetailResponse
	body := projectionRead(t, f, "/runs/"+receipt.ID, &old)
	configureQueue(t, f, 0, 7)
	q := configuredQueue(t, f)
	if len(q.Items) != 1 || q.Items[0].RunCount != 1 || q.Items[0].State != "unknown" {
		t.Fatal(q)
	}
	var detail reviews.RunDetailResponse
	if !bytes.Equal(body, projectionRead(t, f, "/runs/"+receipt.ID+"?snapshot="+old.SnapshotID, &detail)) {
		t.Fatal("promoting selection changed original review")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_current_runs WHERE project_id=?`, attemptProject) != 1 {
		t.Fatal("review duplicated")
	}
}
func TestExecutionQueuePlaceholderIsObservedWithoutSecondEnrollment(t *testing.T) {
	f := queueEnrollmentFixture(t)
	configureQueue(t, f, 0, 2)
	clock := &lifecycleTestClock{now: time.Now().UTC().Add(time.Minute)}
	options, _ := lifecycleOptions([]LifecycleSyncOptions{{Now: clock.Now}})
	r := &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { return lifecyclePull(2), nil }}}
	if !lifecycleTick(t, f, newLifecycleSchedule(options), r) {
		t.Fatal("configuration placeholder not automatically observed")
	}
	got := lifecycleRead(t, f)
	if got.Title != lifecyclePull(2).Title || got.HeadSHA != lifecyclePull(2).HeadSHA || got.State != "open" || r.calls.Load() == 0 {
		t.Fatal(got)
	}
}

// This maximum-size transaction is qualified without race instrumentation,
// like the other ordinary-runtime deadline gates. Smaller queue contracts and
// concurrent mutation/rollback paths remain in the race suite.
func TestExecutionQueueMaximumUsesOrdinaryWriteBudget(t *testing.T) {
	f := queueEnrollmentFixture(t)
	numbers := make([]int64, 100)
	for i := range numbers {
		numbers[i] = int64(i + 1)
	}
	started := time.Now()
	configureQueue(t, f, 0, numbers...)
	enrollmentTime := time.Since(started)
	slices.Reverse(numbers)
	started = time.Now()
	configureQueue(t, f, 1, numbers...)
	t.Logf("100 PRs, 200 default channel controls: enrollment=%s reorder=%s; ordinary mutation deadlines unchanged", enrollmentTime, time.Since(started))
	q := configuredQueue(t, f)
	if !reflect.DeepEqual(queueNumbers(q), numbers) || q.HasMore {
		t.Fatal("bounded maximum queue not published in order", q)
	}
	if err := reviews.ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}
func TestExecutionQueueDoesNotReactivateHistoricallyRetiredMissingEntry(t *testing.T) {
	f := queueEnrollmentFixture(t)
	configureQueue(t, f, 0, 2)
	q := configuredQueue(t, f)
	req := reviews.QueueEditRequest{ExpectedRevision: &q.Revision, RepositoryID: 801, Source: "operator", Queue: []reviews.QueuePullInput{}, Retired: []reviews.RetiredPull{{Number: 2, Reason: "defunct"}}}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/queue/edits", f.admin, "retired", encoded(req))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	q = configuredQueue(t, f)
	obs := reviews.ObservationRequest{ExpectedRevision: &q.Revision, RepositoryID: 801, Source: "replacement", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{projectionPull(4)}, Retired: []reviews.RetiredPull{{Number: 2, Reason: "defunct"}}}
	w = f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "replacement", encoded(obs))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	q = configuredQueue(t, f)
	obs.ExpectedRevision = &q.Revision
	obs.Retired = []reviews.RetiredPull{}
	obs.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	w = f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "clear-current-retirement", encoded(obs))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	configureQueue(t, f, 1, 2, 4)
	if !reflect.DeepEqual(queueNumbers(configuredQueue(t, f)), []int64{4}) {
		t.Fatal("historical retirement disappeared")
	}
	configureQueue(t, f, 2, 4)
	_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "retired-new-target", enrollmentConfiguration(3, []int64{2, 4}, []string{"cli"}))
	attemptCode(t, err, "review_target_retired")
}
