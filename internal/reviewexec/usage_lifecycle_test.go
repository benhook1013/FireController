package reviewexec

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

// This joined usage check deliberately crosses the normally isolated selection,
// execution, adjudication, automatic observation and stored browser boundaries.
// All source/provider records are synthetic; no provider adapter is installed.
func TestUsageLifecycleSelectionRestartAdjudicationExternalAndMerge(t *testing.T) {
	f := selectionFixture(t)
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	var err error
	f.e, err = New(f.store, attemptCapabilities, WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	usageLifecycleHandler(f)
	trap := &cliPublicationHTTPTrap{}
	oldTransport, oldClient := http.DefaultTransport, http.DefaultClient
	http.DefaultTransport, http.DefaultClient = trap, &http.Client{Transport: trap}
	t.Cleanup(func() { http.DefaultTransport, http.DefaultClient = oldTransport, oldClient })
	roundGrant(t, f, 2, "cli", 0, 0, 1, "usage-one-cli")

	reader := &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		p := lifecyclePull(n)
		p.ChangedFiles = 1
		return p, nil
	}}
	selectedReceipt, selectedBytes := selectionEnqueue(t, f, "usage-selection", "cli", nil)
	selectionNext(t, f, reader)
	selected := selectionRead(t, f, selectedReceipt.ID)
	if selected.State != "selected" || selected.OperationID == "" {
		t.Fatal("normal selection did not choose queue head", selected)
	}
	if roundOperation(t, f, selected.OperationID).PullNumber != 2 {
		t.Fatal("selected operation was not queue head")
	}
	a := f.start(t, attemptProject, Receipt{ID: selected.OperationID})
	anchor := attemptAnchor(attemptProject, 2)
	anchor.ParentSHA = anchor.BaseSHA
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("1", 64)
	input := ReviewEvidenceInput{
		CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1024,
		StartedAt:  time.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339Nano),
		FinishedAt: time.Now().Add(-9 * time.Second).UTC().Format(time.RFC3339Nano), ExpectedAnchor: anchor,
		Review: reviewprovider.ParsedReview{Outcome: "completed", Complete: true, ReportedFindings: 1,
			ReviewedFiles: []string{"changed.go"}, Findings: []reviewprovider.ParsedFinding{{Title: "Original correction", Detail: "Original immutable source text", Severity: "high", Path: "changed.go", StartLine: 1, EndLine: 1}}},
	}
	captured, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	pull := projectionPull(2)
	if _, err = f.e.PublishEvidence(t.Context(), attemptProject, captured.ID, &pull); err != nil {
		t.Fatal(err)
	}
	adjudicationComplete(t, f, a)
	decision := FindingDecisionRequest{ExpectedRevision: adjudicationRevision(0), Finding: 0, Disposition: "accepted", Reason: "Verified original finding"}
	firstDecision, err := f.e.DecideReviewFinding(t.Context(), f.scope(t, attemptProject, f.writer, false), "usage-decision", captured.ID, decision)
	if err != nil {
		t.Fatal(err)
	}
	var old reviews.PullResponse
	oldPullBytes := projectionRead(t, f, "/pulls/2", &old)
	oldDetail := cliPublicationDetail(t, f, 2, captured.ID)
	if oldDetail.Run.Finalized || oldDetail.Run.Fixed != 0 || oldDetail.Run.Accepted != 1 {
		t.Fatal("pre-restart browser state fabricated completion", oldDetail.Run)
	}
	// A completed provider attempt with unfinished correction must still block
	// channel progression even though its one-round maximum is allocated.
	plan := SourcePlan{Repository: reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, Pull: lifecyclePull(2), ParentSHA: anchor.ParentSHA, ObservedAt: stamp()}
	plan.Pull.ChangedFiles = 1
	if complete, err := f.e.DiscoveryComplete(t.Context(), attemptProject, plan, "cli"); err != nil || complete {
		t.Fatal("unfinished accepted finding released queue head", complete, err)
	}

	usageLifecycleReopen(t, f)
	before := selectionCounts(t, f)
	replayed, err := f.e.EnqueueRun(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "usage-selection", RunRequest{Channel: "cli"})
	if err != nil || !bytes.Equal(replayed, selectedBytes) || selectionCounts(t, f) != before {
		t.Fatal("lost selection receipt could not recover unchanged after restart", err)
	}
	replayed, err = f.e.DecideReviewFinding(t.Context(), f.scope(t, attemptProject, f.writer, false), "usage-decision", captured.ID, decision)
	if err != nil || !bytes.Equal(replayed, firstDecision) || selectionCounts(t, f) != before {
		t.Fatal("lost decision receipt replay mutated state after restart", err)
	}
	fix := FindingFixRequest{ExpectedRevision: adjudicationRevision(1), Finding: 0, ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("f", 40), ProofNote: "Synthetic focused fix proof"}
	if _, err = f.e.RecordFindingFix(t.Context(), f.scope(t, attemptProject, f.writer, false), "usage-fix", captured.ID, fix); err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "usage-finalize", captured.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(2)}); err != nil {
		t.Fatal(err)
	}
	native := cliPublicationDetail(t, f, 2, captured.ID)
	if !native.Run.Finalized || native.Run.Accepted != 1 || native.Run.Fixed != 1 || native.Findings[0].Detail != input.Review.Findings[0].Detail {
		t.Fatal("resumed control writes lost finalized correction or source", native)
	}
	if round := roundRead(t, f, 2, "cli"); round.Used != 1 || !roundDone(round) {
		t.Fatal("finalization did not consume exactly its original round", round)
	}
	if complete, err := f.e.DiscoveryComplete(t.Context(), attemptProject, plan, "cli"); err != nil || !complete {
		t.Fatal("completed correction did not release channel", complete, err)
	}
	selection, _ := selectionEnqueue(t, f, "usage-next-selection", "cli", nil)
	selectionNext(t, f, reader)
	if next := selectionRead(t, f, selection.ID); next.State != "selected" || roundOperation(t, f, next.OperationID).PullNumber != 4 {
		t.Fatal("next request did not advance to next PR", next)
	}

	// Independent external Hosted history is collected without a request and
	// without silently receiving native coverage or consuming native allowance.
	external := &usageLifecycleExternalReader{externalReader(externalSnapshot(1, anchor.CandidateSHA))}
	external.pull = func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) { return lifecyclePull(n), nil }
	external.snapshot.Reviews[0].SubmittedAt = time.Now().Add(-time.Second).UTC()
	external.snapshot.ObservedAt = time.Now().UTC()
	clock := &lifecycleTestClock{now: time.Now()}
	options, err := lifecycleOptions([]LifecycleSyncOptions{{Now: clock.Now}})
	if err != nil {
		t.Fatal(err)
	}
	schedule := newLifecycleSchedule(options)
	actionTables := []string{"review_selections", "review_operations", "review_execution_attempts", "receipts"}
	actions := make([]int, len(actionTables))
	for i, table := range actionTables {
		actions[i] = attemptCount(t, f.store, "SELECT count(*) FROM "+table)
	}
	rounds := roundRead(t, f, 2, "cli")
	// The observer selects both enrolled PRs; drain one finite pass.
	for i := 0; i < 2; i++ {
		lifecycleTick(t, f, schedule, external)
	}
	if externalArchiveCount(t, f) != 1 || roundRead(t, f, 2, "cli") != rounds {
		t.Fatal("external pickup duplicated history or changed native rounds", externalArchiveCount(t, f), roundRead(t, f, 2, "cli"), rounds, schedule.due, schedule.accounts)
	}
	var current reviews.PullResponse
	projectionRead(t, f, "/pulls/2", &current)
	if current.Pull.RunCounts != (reviews.ChannelRunCounts{Hosted: 1, CLI: 1}) {
		t.Fatal("shared browser read lost channel totals", current.Pull)
	}
	assessment, err := f.e.ReadPolicyAssessment(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), captured.ID)
	if err != nil || assessment.State != "held" || len(assessment.Channels) != 2 || !assessment.Channels[0].Covered || assessment.Channels[1].Covered {
		t.Fatal("external archive fabricated missing native Hosted coverage", assessment, err)
	}
	// Observer writes events/snapshots, but never execution actions or receipts.
	for i, table := range actionTables {
		if count := attemptCount(t, f.store, "SELECT count(*) FROM "+table); count != actions[i] {
			t.Fatal("passive observer created action/receipt", table, actions[i], count)
		}
	}
	usageLifecycleReopen(t, f)
	schedule = newLifecycleSchedule(options)
	for i := 0; i < 2; i++ {
		lifecycleTick(t, f, schedule, external)
	}
	if externalArchiveCount(t, f) != 1 {
		t.Fatal("actual store restart duplicated external run")
	}
	clock.Add(time.Minute)
	external.pull = func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		p := lifecyclePull(n)
		if n == 2 {
			p.State, p.Merged, p.MergedAt = "closed", true, time.Now().UTC().Format(time.RFC3339Nano)
		}
		return p, nil
	}
	for i := 0; i < 2; i++ {
		lifecycleTick(t, f, schedule, external)
	}
	sourceCalls, archiveCalls := external.calls.Load(), external.archiveCalls
	projectionRead(t, f, "/pulls/2", &current)
	if current.Pull.State != "merged" || current.Pull.RunCounts != (reviews.ChannelRunCounts{Hosted: 1, CLI: 1}) || cliPublicationDetail(t, f, 2, captured.ID).Run.Fixed != 1 {
		t.Fatal("automatic merge observation lost completed review history", current.Pull)
	}
	var queue reviews.QueueResponse
	projectionRead(t, f, "/queue?view=recent", &queue)
	retainedMerged := false
	for _, item := range queue.Items {
		if item.Number == 2 && item.State == "merged" && item.RunCount == 2 {
			retainedMerged = true
		}
	}
	if !retainedMerged {
		t.Fatal("recent queue dropped newly merged review history", queue)
	}
	var pinned reviews.PullResponse
	if got := projectionRead(t, f, "/pulls/2?snapshot="+old.SnapshotID, &pinned); !bytes.Equal(got, oldPullBytes) {
		t.Fatal("later correction/observation changed pinned pre-restart snapshot")
	}
	if external.calls.Load() != sourceCalls || external.archiveCalls != archiveCalls {
		t.Fatal("stored browser reads performed provider observation")
	}
	if trap.calls.Load() != 0 {
		t.Fatal("synthetic lifecycle unexpectedly attempted external HTTP", trap.calls.Load())
	}
}

func usageLifecycleHandler(f *attemptFixture) {
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, f.store)
	f.handler = mux
}

func usageLifecycleReopen(t *testing.T, f *attemptFixture) {
	t.Helper()
	var sequence int
	var name, path string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	instance := f.store.InstanceID()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(t.Context(), filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	f.store = reopened
	if reopened.InstanceID() != instance {
		t.Fatal("fixture restart changed installation identity")
	}
	f.e, err = New(reopened, attemptCapabilities, WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	usageLifecycleHandler(f)
}

// Keep provider inventories scoped to their selected PR, including the second
// enrolled PR that is observed in the same account pass.
type usageLifecycleExternalReader struct{ *externalTestReader }

func (r *usageLifecycleExternalReader) ObserveHosted(ctx context.Context, repository string, number int64) (reviewprovider.HostedPublicationSnapshot, error) {
	if number == 2 {
		return r.externalTestReader.ObserveHosted(ctx, repository, number)
	}
	r.archiveCalls++
	return externalSnapshot(0, strings.Repeat("a", 40)), nil
}
