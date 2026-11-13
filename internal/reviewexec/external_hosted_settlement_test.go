package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestExternalHostedSettlementCountsThreeSameHeadResultsOnce(t *testing.T) {
	f, schedule, clock := lifecycleFixture(t)
	roundGrant(t, f, 2, "hosted", 0, 2, 3, "manual-results-grant")
	head := strings.Repeat("a", 40)
	reader := externalReader(externalSnapshot(3, head))
	lifecycleTick(t, f, schedule, reader)

	scope := f.scope(t, attemptProject, f.writer, false)
	var history reviews.RunsResponse
	projectionRead(t, f, "/pulls/2/runs", &history)
	if len(history.Items) != 3 || roundRead(t, f, 2, "hosted").Used != 0 {
		t.Fatal("capture must retain three results without accounting credit", history.Items)
	}
	f.configure(t, attemptProject, "test-capability", 2, false)
	request := Request{ExpectedConfigurationRevision: 3, PullNumber: 2, Channel: "hosted", HeadSHA: strings.Repeat("b", 40)}
	_, err := f.e.Enqueue(t.Context(), scope, "blocked-by-manual-results", request)
	var held *core.Error
	if !errors.As(err, &held) || held.Code != "external_hosted_pending" {
		t.Fatal("pending external results must hold Hosted even after a head change", err)
	}
	details, ok := held.Details.(map[string]any)
	if !ok || len(details["run_ids"].([]string)) != 3 {
		t.Fatal("hold does not provide the selected PR's pending result handles", held)
	}
	checkFront := func(ctx context.Context, tx *sql.Tx) error {
		hosted, err := channelProgress(ctx, tx, attemptProject, 801, 2, "hosted")
		if err != nil {
			return err
		}
		cli, err := channelProgress(ctx, tx, attemptProject, 801, 2, "cli")
		if err != nil {
			return err
		}
		if !hosted.Pending || !hosted.ExternalPending || cli.Pending {
			t.Fatal("external obligation did not stay within the Hosted channel", hosted, cli)
		}
		return nil
	}
	if err := f.store.Read(t.Context(), scope, checkFront); err != nil {
		t.Fatal(err)
	}
	count := true
	batch := ExternalHostedSettlement{CountRound: &count}
	for _, run := range history.Items {
		batch.Runs = append(batch.Runs, ExternalHostedSelectedRun{RunID: run.ID, Findings: []ExternalHostedDecision{}})
	}
	got, err := f.e.SettleExternalHosted(t.Context(), scope, "settle-all", 2, batch)
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		State        string `json:"state"`
		Runs         int    `json:"runs"`
		RoundCounted bool   `json:"round_counted"`
	}
	if err := json.Unmarshal(got, &receipt); err != nil || receipt.State != "external_hosted_settled" || receipt.Runs != 3 || !receipt.RoundCounted {
		t.Fatal("invalid settlement receipt", string(got), err)
	}
	replay, err := f.e.SettleExternalHosted(t.Context(), scope, "settle-all", 2, batch)
	if err != nil || !bytes.Equal(got, replay) {
		t.Fatal("lost response did not return the original receipt", err)
	}
	if _, err = f.e.SettleExternalHosted(t.Context(), scope, "overlap-new-key", 2, batch); err == nil {
		t.Fatal("already settled runs earned another credit")
	}
	current := roundRead(t, f, 2, "hosted")
	if current.Used != 1 || current.Dry != 1 || current.Tapered {
		t.Fatal("three manual results must consume one round and one clean step", current)
	}
	if err = ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal("round replay does not verify settled evidence", err)
	}
	clock.Add(schedule.options.OpenInterval)
	lifecycleTick(t, f, schedule, reader)
	projectionRead(t, f, "/pulls/2/runs", &history)
	if len(history.Items) != 3 {
		t.Fatal("observer replay duplicated settled runs", history.Items)
	}
	for _, run := range history.Items {
		if !run.Finalized || run.HeadSHA != head {
			t.Fatal("settlement lost original comparison or adjudication", run)
		}
	}
	if _, err = f.e.Enqueue(t.Context(), scope, "after-manual-results", request); err != nil {
		t.Fatal("settlement did not release the Hosted hold", err)
	}
}

func TestExternalHostedSettlementRequiresFixProofAndRollsBackWholeBatch(t *testing.T) {
	f, schedule, _ := lifecycleFixture(t)
	roundGrant(t, f, 2, "hosted", 0, 2, 3, "manual-results-grant")
	head := strings.Repeat("a", 40)
	snapshot := externalSnapshot(3, head)
	snapshot.Reviews[2].Body = strings.Replace(snapshot.Reviews[2].Body, "posted: 0", "posted: 1", 1)
	at := snapshot.Reviews[2].SubmittedAt
	reader := externalReader(snapshot)
	reader.snapshot.ReviewComments = append(reader.snapshot.ReviewComments, reviewprovider.ReviewComment{
		ID: 901, ReviewID: 403, Author: reader.TrustedHostedActor(), HeadSHA: head, OriginalHeadSHA: head,
		Path: "fixture.go", CreatedAt: at, UpdatedAt: at,
		Body: "_⚠️ Potential issue_ | _🟠 Major_\n\n**Fixture finding.**\n\nSource explanation.\n\n<!-- cr-comment:v1:000000000000000000000001 -->",
	})
	lifecycleTick(t, f, schedule, reader)
	scope := f.scope(t, attemptProject, f.writer, false)
	var history reviews.RunsResponse
	projectionRead(t, f, "/pulls/2/runs", &history)
	if len(history.Items) != 3 {
		t.Fatal("fixture missing captured results", history.Items)
	}
	count := true
	positive := ""
	var clean []string
	for _, run := range history.Items {
		if run.Found == 1 {
			positive = run.ID
		} else {
			clean = append(clean, run.ID)
		}
	}
	if positive == "" || len(clean) != 2 {
		t.Fatal("fixture did not retain one positive result")
	}
	batch := ExternalHostedSettlement{CountRound: &count, Runs: []ExternalHostedSelectedRun{
		{RunID: clean[0], Findings: []ExternalHostedDecision{}},
		{RunID: positive, Findings: []ExternalHostedDecision{}},
	}}
	if _, err := f.e.SettleExternalHosted(t.Context(), scope, "incomplete", 2, batch); err == nil {
		t.Fatal("settlement discarded undecided findings")
	}
	projectionRead(t, f, "/pulls/2/runs", &history)
	for _, run := range history.Items {
		if run.Finalized {
			t.Fatal("invalid second result leaked the first run's settlement")
		}
	}
	if _, err := f.e.SettleExternalHosted(t.Context(), scope, "one-clean", 2, ExternalHostedSettlement{CountRound: &count, Runs: batch.Runs[:1]}); err != nil {
		t.Fatal(err)
	}
	var detail reviews.RunDetailResponse
	projectionRead(t, f, "/runs/"+positive, &detail)
	if len(detail.Findings) != 1 {
		t.Fatal("fixture finding missing")
	}
	batch.Runs = batch.Runs[1:]
	batch.Runs[0].Findings = []ExternalHostedDecision{{FindingID: detail.Findings[0].SourceFindingID, Disposition: "accepted", Reason: "Verified correction."}}
	if _, err := f.e.SettleExternalHosted(t.Context(), scope, "missing-fix", 2, batch); err == nil {
		t.Fatal("accepted finding escaped the fix hold")
	}
	batch.Runs[0].Findings[0].FixSHA = strings.Repeat("c", 40)
	batch.Runs[0].Findings[0].ProofNote = "Verified source correction and focused check."
	batch.Runs = append(batch.Runs, ExternalHostedSelectedRun{RunID: clean[1], Findings: []ExternalHostedDecision{}})
	if _, err := f.e.SettleExternalHosted(t.Context(), scope, "fixed-batch", 2, batch); err != nil {
		t.Fatal(err)
	}
	current := roundRead(t, f, 2, "hosted")
	if current.Used != 2 || current.Dry != 0 || current.Tapered {
		t.Fatal("an accepted finding must reset taper even beside a clean result", current)
	}
}

func TestExternalHostedResultAppearingAfterPreparationHoldsDispatch(t *testing.T) {
	hostedAdmissionNoHTTP(t)
	f, schedule, _ := lifecycleFixture(t)
	f.configure(t, attemptProject, "test-capability", 2, false)
	anchor := attemptAnchor(attemptProject, 2)
	admitted := hostedAdmissionAttempt(t, f, "before-observation", "hosted", 3, anchor)
	hostedAdmissionVerify(t, f, admitted, "", "")

	lifecycleTick(t, f, schedule, externalReader(externalSnapshot(1, anchor.CandidateSHA)))
	hostedAdmissionVerify(t, f, admitted, "external_hosted_pending", "")
}

func TestExternalHostedSettlementWithoutCreditUsesProjectWriteScope(t *testing.T) {
	f, schedule, _ := lifecycleFixture(t)
	roundGrant(t, f, 2, "hosted", 0, 2, 3, "manual-results-grant")
	lifecycleTick(t, f, schedule, externalReader(externalSnapshot(1, strings.Repeat("a", 40))))
	var history reviews.RunsResponse
	projectionRead(t, f, "/pulls/2/runs", &history)
	if len(history.Items) != 1 {
		t.Fatal("missing capture", history.Items)
	}
	path := "/api/v1/projects/" + attemptProject + "/reviews/external-hosted/2/settlement"
	count := false
	request := ExternalHostedSettlement{CountRound: &count, Runs: []ExternalHostedSelectedRun{{RunID: history.Items[0].ID, Findings: []ExternalHostedDecision{}}}}
	if got := f.request("POST", path, f.reader, "reader-refused", encoded(request)); got.Code != 403 {
		t.Fatal("reader must not settle results", got.Code)
	}
	if got := f.request("POST", path, f.writer, "settle-without-credit", encoded(request)); got.Code != 200 {
		t.Fatal("project writer could not settle without consuming allocation", got.Code, got.Body.String())
	}
	if state := roundRead(t, f, 2, "hosted"); state.Used != 0 || state.Dry != 0 {
		t.Fatal("no-credit settlement changed accounting", state)
	}
	projectionRead(t, f, "/pulls/2/runs", &history)
	if len(history.Items) != 1 || !history.Items[0].Finalized {
		t.Fatal("settlement failed to record the original result as handled", history.Items)
	}
}

func TestExternalHostedCaptureDuringPreparationReleasesReservation(t *testing.T) {
	f, schedule, _ := lifecycleFixture(t)
	f.configure(t, attemptProject, "test-capability", 2, false)
	anchor := attemptAnchor(attemptProject, 2)
	op := f.enqueue(t, attemptProject, "hosted-preparation-race", "hosted", 2, 3)
	driver := &controlledDriver{prepare: func(_ context.Context, _ Operation, _ Configuration, _ Attempt) (Preparation, error) {
		lifecycleTick(t, f, schedule, externalReader(externalSnapshot(1, anchor.CandidateSHA)))
		return Preparation{Anchor: anchor, Intent: "synthetic-unposted-intent"}, nil
	}}
	if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
		t.Fatal(err)
	}
	after := roundOperation(t, f, op.ID)
	if after.State != "held" || after.Reason != "external_hosted_pending" || driver.prepares.Load() != 1 || driver.calls.Load() != 0 {
		t.Fatal("unexpected operation after late capture", after)
	}
	if got := attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`); got != 0 {
		t.Fatal("late capture stranded a reservation", got)
	}
}

func TestExternalHostedCaptureReplacesQueuedIntent(t *testing.T) {
	f, schedule, _ := lifecycleFixture(t)
	f.configure(t, attemptProject, "test-capability", 2, false)
	op := f.enqueue(t, attemptProject, "hosted-queued-before-capture", "hosted", 2, 3)
	lifecycleTick(t, f, schedule, externalReader(externalSnapshot(1, strings.Repeat("a", 40))))
	after := roundOperation(t, f, op.ID)
	if after.State != "held" || after.Reason != "external_hosted_pending" || after.AttemptID != "" {
		t.Fatal("old request remained runnable after capture", after)
	}
}
