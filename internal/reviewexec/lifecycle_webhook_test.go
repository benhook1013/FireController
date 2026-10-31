package reviewexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/githubhook"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func attachLifecycleHints(t *testing.T, f *attemptFixture, s *lifecycleSchedule) {
	t.Helper()
	f.e.lifecycleMu.Lock()
	f.e.lifecycleSchedule = s
	f.e.lifecycleMu.Unlock()
	f.e.lifecycleRunning.Store(true)
	t.Cleanup(func() { f.e.lifecycleRunning.Store(false) })
}

func hintLifecycle(t *testing.T, f *attemptFixture, hint githubhook.Hint) {
	t.Helper()
	if err := f.e.HintGitHub(t.Context(), hint); err != nil {
		t.Fatal(err)
	}
}

func lifecycleHintCounts(t *testing.T, f *attemptFixture) []int {
	t.Helper()
	var out []int
	for _, table := range []string{"grants", "review_operations", "review_execution_attempts", "review_hosted_allowance_queries", "review_execution_config", "review_snapshots", "review_round_completions"} {
		out = append(out, attemptCount(t, f.store, "SELECT count(*) FROM "+table))
	}
	return out
}

func TestLifecycleWebhookCoalescesScopedHintsAndForcesFreshCI(t *testing.T) {
	f, s, clock := lifecycleFixture(t)
	attachLifecycleHints(t, f, s)
	pull := lifecyclePull(2)
	r := externalReader(externalSnapshot(0, pull.HeadSHA))
	r.pull = func(context.Context, string, int64) (reviewprovider.Pull, error) { return pull, nil }
	readers := map[string]TargetReader{"test-capability": r}
	lifecycleTick(t, f, s, r)
	key := lifecycleKey{attemptProject, 2}
	initial := s.due[key]
	before := lifecycleHintCounts(t, f)
	for _, hint := range []githubhook.Hint{
		{RepositoryID: 802, PullNumbers: []int64{2}, Metadata: true, CI: true, HeadSHA: pull.HeadSHA},
		{RepositoryID: 801, PullNumbers: []int64{4}, Metadata: true}, // Visible, but not enrolled.
		{RepositoryID: 801, PullNumbers: []int64{999}, Metadata: true},
		{RepositoryID: 801, PullNumbers: []int64{2}, CI: true, HeadSHA: strings.Repeat("c", 40)},
		{RepositoryID: 801, BranchRef: "foreign-base", Metadata: true, CI: true},
		{RepositoryID: 801, BaseRef: "main", Metadata: true, CI: true},
		{RepositoryID: 801, Metadata: true, CI: true},
	} {
		hintLifecycle(t, f, hint)
	}
	if s.due[key] != initial || r.calls.Load() != 4 || r.ciCalls.Load() != 1 || !reflect.DeepEqual(before, lifecycleHintCounts(t, f)) {
		t.Fatal("irrelevant hints changed scheduling, database or provider reads")
	}
	for range 20 {
		hintLifecycle(t, f, githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{2}, Metadata: true, HeadSHA: strings.Repeat("d", 40)})
	}
	if !reflect.DeepEqual(before, lifecycleHintCounts(t, f)) || r.calls.Load() != 4 {
		t.Fatal("delivery itself wrote state or fetched provider facts")
	}
	target, err := f.e.claimLifecycle(t.Context(), readers, s)
	if err != nil || target == nil || target.hintGeneration != 20 || target.forceCI {
		t.Fatal("metadata burst did not coalesce into cheap lane", target, err)
	}
	if another, err := f.e.claimLifecycle(t.Context(), readers, s); err != nil || another != nil {
		t.Fatal("in-flight project/account reservation bypassed", another, err)
	}
	// A new hint during this read must survive completion of its claimed generation.
	hintLifecycle(t, f, githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{2}, Metadata: true})
	if err := f.e.observeLifecycle(t.Context(), *target, s); err != nil {
		t.Fatal(err)
	}
	if d := s.due[key].hints[lifecycleMetadata]; d.done != 20 || d.wanted != 21 {
		t.Fatal("completion lost in-flight delivery", d)
	}
	if next, err := f.e.claimLifecycle(t.Context(), readers, s); err != nil || next != nil {
		t.Fatal("delivery burst ignored five-second floor", next, err)
	}
	clock.Add(5*time.Second - time.Nanosecond)
	if next, err := f.e.claimLifecycle(t.Context(), readers, s); err != nil || next != nil {
		t.Fatal("delivery ran before exact floor", next, err)
	}
	clock.Add(time.Nanosecond)
	target, err = f.e.claimLifecycle(t.Context(), readers, s)
	if err != nil || target == nil || target.hintGeneration != 21 {
		t.Fatal("in-flight delivery did not get next eligible read", target, err)
	}
	if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil || r.ciCalls.Load() != 1 {
		t.Fatal("metadata hint refreshed CI", err)
	}
	if !clock.Now().Before(s.due[key].ci) {
		t.Fatal("fixture no longer proves early CI force")
	}
	retainedHealth := s.due[key]
	retainedHealth.enrichmentError, retainedHealth.hostedError = "external_hosted_identity_unproven", "external_hosted_identity_unproven"
	s.due[key] = retainedHealth
	normalEnrichment, hostedChecked := s.due[key].enrichmentNext, s.due[key].hostedCheckedAt
	for range 20 {
		hintLifecycle(t, f, githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{2}, HeadSHA: pull.HeadSHA, CI: true})
	}
	r.ci = func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		return sourceCITestObservation(target, "failed"), nil
	}
	target, err = f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
	if err != nil || target == nil || !target.forceCI || !target.ciOnly || target.hintGeneration != 20 {
		t.Fatal("CI event did not force existing enrichment lane", target, err)
	}
	if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil || r.ciCalls.Load() != 2 || lifecycleRead(t, f).CIState != "failure" {
		t.Fatal("due CI event did not collect actual new CI facts", err)
	}
	if next, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment); err != nil || next != nil {
		t.Fatal("coalesced CI delivery kept polling", next, err)
	}
	if r.archiveCalls != 1 || s.due[key].enrichmentNext != normalEnrichment || s.due[key].hostedCheckedAt != hostedChecked || s.due[key].hostedError != retainedHealth.hostedError || s.due[key].enrichmentError != retainedHealth.enrichmentError {
		t.Fatal("CI-only event fetched Hosted history or delayed its ordinary cadence", r.archiveCalls, s.due[key])
	}
	clock.Add(normalEnrichment.Sub(clock.Now()))
	target, err = f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
	if err != nil || target == nil || target.ciOnly || target.forceCI {
		t.Fatal("normal Hosted observation was delayed by CI events", target, err)
	}
	if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil || r.archiveCalls != 2 || r.ciCalls.Load() != 2 {
		t.Fatal("ordinary enrichment did not retain Hosted cadence independently of CI", err, r.archiveCalls, r.ciCalls.Load())
	}
	// Push selector targets the stored base branch, even when its new tip is
	// different from this PR's head. Payload values never update that head.
	hintLifecycle(t, f, githubhook.Hint{RepositoryID: 801, BranchRef: "main", HeadSHA: strings.Repeat("e", 40), Metadata: true, CI: true})
	if s.due[key].hints[lifecycleMetadata].wanted != 22 || s.due[key].hints[lifecycleEnrichment].wanted != 21 || lifecycleRead(t, f).HeadSHA != pull.HeadSHA {
		t.Fatal("base push lost scope or supplied provider truth")
	}
	for _, table := range []string{"review_operations", "review_execution_attempts", "review_hosted_allowance_queries", "review_round_completions"} {
		if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("hints or observation created review authority", table)
		}
	}
}

func TestLifecycleWebhookKeepsCooldownAndRejectsUnavailableOrStaleCI(t *testing.T) {
	f, s, clock := lifecycleFixture(t)
	attachLifecycleHints(t, f, s)
	r := &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { return lifecyclePull(2), nil }}}
	readers := map[string]TargetReader{"test-capability": r}
	lifecycleTick(t, f, s, r)
	key := lifecycleKey{attemptProject, 2}
	hint := githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{2}, Metadata: true}
	hintLifecycle(t, f, hint)
	target, err := f.e.claimLifecycle(t.Context(), readers, s)
	if err != nil || target == nil {
		t.Fatal(target, err)
	}
	s.finish(*target, "open", "", false, false, &reviewprovider.Error{Code: "provider_rate_limited", RetryAfterSeconds: 120})
	hintLifecycle(t, f, hint)
	clock.Add(5 * time.Second)
	for _, lane := range []lifecycleLane{lifecycleMetadata, lifecycleEnrichment} {
		if next, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane); err != nil || next != nil {
			t.Fatal("event bypassed shared account cooldown", next, err)
		}
	}
	clock.Add(115 * time.Second)
	target, err = f.e.claimLifecycle(t.Context(), readers, s)
	if err != nil || target == nil || target.hintGeneration != 2 {
		t.Fatal("hint lost while account backed off", target, err)
	}
	if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil {
		t.Fatal(err)
	}
	// Make normal enrichment not due, enqueue a current-head CI hint, then
	// move just the stored comparison. That old hint must not acquire new CI.
	d := s.due[key]
	d.enrichmentHead, d.enrichmentBaseSHA, d.enrichmentBaseRef = d.head, d.baseSHA, d.baseRef
	d.enrichmentNext = clock.Now().Add(time.Hour)
	s.due[key] = d
	hintLifecycle(t, f, githubhook.Hint{RepositoryID: 801, HeadSHA: strings.Repeat("a", 40), CI: true})
	if _, err = f.store.DB().Exec(`UPDATE review_pull_versions SET head_sha=? WHERE project_id=? AND number=2`, strings.Repeat("f", 40), attemptProject); err != nil {
		t.Fatal(err)
	}
	before := r.calls.Load()
	if next, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment); err != nil || next != nil || r.calls.Load() != before || s.due[key].hints[lifecycleEnrichment].wanted != s.due[key].hints[lifecycleEnrichment].done {
		t.Fatal("stale queued CI hint acquired different head", next, err)
	}
	f.e.lifecycleRunning.Store(false)
	if err = f.e.HintGitHub(t.Context(), hint); !lifecycleHintOverloaded(err) {
		t.Fatal("unavailable scheduler accepted live hint", err)
	}
	if err = f.e.HintGitHub(t.Context(), githubhook.Hint{RepositoryID: 9999, Metadata: true, PullNumbers: []int64{2}}); err != nil {
		t.Fatal("unknown repository did not remain a no-op", err)
	}
	f.e.lifecycleRunning.Store(true)
	delete(s.due, key)
	for n := range 64 * 100 {
		s.due[lifecycleKey{"bounded-other-project", int64(n + 1)}] = lifecycleDue{}
	}
	if err = f.e.HintGitHub(t.Context(), hint); !lifecycleHintOverloaded(err) {
		t.Fatal("unbounded scheduling map accepted", err)
	}
	if got := fmt.Sprint(err); strings.Contains(got, "PRIVATE") {
		t.Fatal("scheduler error leaked private state")
	}
}

func TestLifecycleWebhookFindsEnrolledPRAfterRetainedHistoryAndIgnoresRetirement(t *testing.T) {
	f := attemptProjectionFixture(t, false)
	var retained []reviews.PullInput
	for n := range 100 {
		pull := projectionPull(int64(1000 + n))
		pull.State = "merged"
		pull.Complete = false
		retained = append(retained, pull)
	}
	base := "/api/v1/projects/" + attemptProject + "/reviews"
	target := projectionPull(2)
	target.Complete = false
	for revision, pulls := range [][]reviews.PullInput{retained, {target}} {
		rev := int64(revision)
		batch := reviews.TransferRequest{Version: 1, ExpectedRevision: &rev, RepositoryID: 801, Publisher: "synthetic-webhook-proof", Groups: []reviews.TransferGroup{{Source: "retained-history", Pulls: pulls}}}
		w := f.request("POST", base+"/transfer/preview", f.admin, "", encoded(batch))
		var preview reviews.TransferPreview
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &preview) != nil {
			t.Fatal("bounded history preview failed", w.Code, w.Body.String())
		}
		w = f.request("POST", base+"/transfer/apply", f.admin, fmt.Sprintf("webhook-history-%d", revision), encoded(reviews.TransferApplyRequest{PreviewSHA256: preview.PreviewSHA256, Batch: batch}))
		if w.Code != 201 {
			t.Fatal("bounded history append failed", w.Code, w.Body.String())
		}
	}
	var position int
	if err := f.store.DB().QueryRow(`SELECT position FROM review_current_pulls WHERE project_id=? AND number=2`, attemptProject).Scan(&position); err != nil || position != 100 {
		t.Fatal("fixture target is not beyond first hundred visible rows", position, err)
	}
	options, _ := lifecycleOptions(nil)
	s := newLifecycleSchedule(options)
	attachLifecycleHints(t, f, s)
	hint := githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{2}, Metadata: true}
	hintLifecycle(t, f, hint)
	key := lifecycleKey{attemptProject, 2}
	if len(s.due) != 1 || s.due[key].hints[lifecycleMetadata].wanted != 1 {
		t.Fatal("history prefix hid enrolled PR or fanned out to history", len(s.due))
	}
	// Visibility retirement is independent from the still-enrolled config.
	revision := int64(2)
	w := f.request("POST", base+"/queue/edits", f.admin, "webhook-retire", encoded(reviews.QueueEditRequest{ExpectedRevision: &revision, RepositoryID: 801, Source: "synthetic-retirement", Queue: []reviews.QueuePullInput{}, Retired: []reviews.RetiredPull{{Number: 2, Reason: "defunct"}}}))
	if w.Code != 201 {
		t.Fatal("canonical retirement failed", w.Code, w.Body.String())
	}
	hintLifecycle(t, f, hint)
	if s.due[key].hints[lifecycleMetadata].wanted != 1 {
		t.Fatal("retired PR accepted another hint")
	}
}

func lifecycleHintOverloaded(err error) bool {
	var failure *core.Error
	return errors.As(err, &failure) && failure.Code == "overloaded" && failure.Status == 503
}
