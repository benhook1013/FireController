package reviewexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	holdmodel "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

const integrationHoldsPath = "/api/v1/projects/exec-project-one/reviews/adoption-obligations"

func integrationHoldRequest(t *testing.T, f *attemptFixture, method, path, token, key string, input any) *httptest.ResponseRecorder {
	t.Helper()
	if _, err := f.store.DB().Exec(`INSERT OR IGNORE INTO workers(project_id,id,name) VALUES(?,'holds-worker','Carried hold integration worker')`, attemptProject); err != nil {
		t.Fatal(err)
	}
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-FC-Worker", "holds-worker")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

// This fixture imports through the registered public contract rather than
// inserting hold rows, so the runtime observes the same retained provenance.
func integrationImportHold(t *testing.T, f *attemptFixture, pull int64) (holdmodel.ApplyRequest, []byte, holdmodel.ListItem) {
	t.Helper()
	zero := int64(0)
	batch := holdmodel.Batch{Version: 1, Source: holdmodel.Source{Namespace: "synthetic-source", RepositoryID: 801, SnapshotSHA256: strings.Repeat("9", 64)}, ExpectedRevision: &zero,
		Obligations: []holdmodel.Obligation{{SourceKey: "finding-original", Kind: "finding", AffectedPullNumbers: []int64{pull}, Summary: "Retained unresolved source finding", Evidence: []string{"source:finding-original"}, OriginalStatus: "pending"}}}
	w := integrationHoldRequest(t, f, "POST", integrationHoldsPath+"/preview", f.admin, "", batch)
	var preview holdmodel.Preview
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &preview) != nil || preview.New != 1 {
		t.Fatalf("hold preview: %d %s", w.Code, w.Body.String())
	}
	input := holdmodel.ApplyRequest{Batch: batch, PreviewSHA256: preview.PreviewSHA256}
	w = integrationHoldRequest(t, f, "POST", integrationHoldsPath+"/apply", f.admin, "holds-import", input)
	var receipt holdmodel.Receipt
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.New != 1 || receipt.Revision != 1 {
		t.Fatalf("hold apply: %d %s", w.Code, w.Body.String())
	}
	original := append([]byte(nil), w.Body.Bytes()...)
	w = integrationHoldRequest(t, f, "GET", integrationHoldsPath, f.reader, "", nil)
	var page holdmodel.Page
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 {
		t.Fatalf("hold list: %d %s", w.Code, w.Body.String())
	}
	return input, original, page.Items[0]
}

func integrationSourceHeld(t *testing.T, err error) {
	t.Helper()
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "review_held" || !reflect.DeepEqual(failure.Details, map[string]string{"reason": "source_obligations_pending"}) {
		t.Fatalf("expected carried-obligation refusal, got %+v", err)
	}
}

func TestAdoptionHoldsIntegrationDirectAdmissionAndIsolation(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	integrationImportHold(t, f, 2)
	for _, channel := range []string{"cli", "hosted"} {
		before := attemptCount(t, f.store, `SELECT count(*) FROM review_operations`)
		_, err := f.e.Enqueue(t.Context(), f.scope(t, attemptProject, f.writer, false), "held-"+channel, Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: channel, HeadSHA: strings.Repeat("a", 40)})
		integrationSourceHeld(t, err)
		if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != before {
			t.Fatal("refused direct admission retained an operation")
		}
		f.enqueue(t, attemptProject, "unrelated-pr-"+channel, channel, 4, 1)
		f.enqueue(t, otherAttemptProject, "unrelated-project-"+channel, channel, 2, 1)
	}
	for _, table := range []string{"review_execution_evidence", "review_round_completions"} {
		if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("import fabricated native review credit", table)
		}
	}
}

func TestAdoptionHoldsIntegrationFindingResolutionCannotReleaseResultAccountingHold(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	zero := int64(0)
	batch := holdmodel.Batch{Version: 1, Source: holdmodel.Source{Namespace: "terminal-source", RepositoryID: 801, SnapshotSHA256: strings.Repeat("9", 64)}, ExpectedRevision: &zero,
		Obligations: []holdmodel.Obligation{
			{SourceKey: "finding-one", Kind: "finding", AffectedPullNumbers: []int64{2}, Summary: "Source correction pending", Evidence: []string{"source:finding-one"}, OriginalStatus: "pending"},
			{SourceKey: "result-one", Kind: "result", AffectedPullNumbers: []int64{2}, Summary: "Terminal source result accounting remains unsettled", Evidence: []string{"source:result-one"}, OriginalStatus: "pending"},
		}}
	w := integrationHoldRequest(t, f, "POST", integrationHoldsPath+"/preview", f.admin, "", batch)
	var preview holdmodel.Preview
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &preview) != nil || preview.New != 2 {
		t.Fatalf("mixed hold preview: %d %s", w.Code, w.Body.String())
	}
	w = integrationHoldRequest(t, f, "POST", integrationHoldsPath+"/apply", f.admin, "mixed-source-holds", holdmodel.ApplyRequest{Batch: batch, PreviewSHA256: preview.PreviewSHA256})
	if w.Code != http.StatusOK {
		t.Fatalf("mixed hold apply: %d %s", w.Code, w.Body.String())
	}
	w = integrationHoldRequest(t, f, "GET", integrationHoldsPath+"?pull_number=2", f.reader, "", nil)
	var page holdmodel.Page
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 2 {
		t.Fatalf("mixed holds read: %d %s", w.Code, w.Body.String())
	}
	ids := map[string]string{}
	for _, item := range page.Items {
		ids[item.Kind] = item.ID
	}
	if ids["finding"] == "" || ids["result"] == "" || ids["finding"] == ids["result"] {
		t.Fatal("mixed obligations lost distinct kinds/identities", page.Items)
	}
	one := int64(1)
	resolution := holdmodel.ResolveRequest{ExpectedRevision: &one, Reason: "Finding correction explicitly verified", Evidence: []string{"commit:source-correction"}}
	w = integrationHoldRequest(t, f, "POST", integrationHoldsPath+"/"+ids["finding"]+"/resolve", f.admin, "resolve-source-finding", resolution)
	if w.Code != http.StatusOK {
		t.Fatalf("finding resolution: %d %s", w.Code, w.Body.String())
	}
	w = integrationHoldRequest(t, f, "GET", integrationHoldsPath+"?pull_number=2", f.reader, "", nil)
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].Kind != "result" || page.Items[0].ID != ids["result"] {
		t.Fatalf("finding resolution lost independent result hold: %d %s", w.Code, w.Body.String())
	}
	for _, channel := range []string{"cli", "hosted"} {
		_, err := f.e.Enqueue(t.Context(), f.scope(t, attemptProject, f.writer, false), "result-held-"+channel, Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: channel, HeadSHA: strings.Repeat("a", 40)})
		integrationSourceHeld(t, err)
	}
	for _, table := range []string{"review_operations", "review_execution_attempts", "review_execution_evidence", "review_round_completions"} {
		if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("result hold refusal fabricated native work or credit", table)
		}
	}
	// This deliberate administrator action states that accounting was verified;
	// the generic hold service neither computes nor creates that accounting.
	resolution.Reason, resolution.Evidence = "Correct destination accounting explicitly verified", []string{"proof:destination-accounting"}
	w = integrationHoldRequest(t, f, "POST", integrationHoldsPath+"/"+ids["result"]+"/resolve", f.admin, "resolve-source-result", resolution)
	if w.Code != http.StatusOK {
		t.Fatalf("result resolution: %d %s", w.Code, w.Body.String())
	}
	for _, channel := range []string{"cli", "hosted"} {
		f.enqueue(t, attemptProject, "result-cleared-"+channel, channel, 2, 1)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 2 {
		t.Fatal("explicit result resolution did not release both admission paths")
	}
	for _, table := range []string{"review_execution_attempts", "review_execution_evidence", "review_round_completions"} {
		if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("result resolution created native execution or completion credit", table)
		}
	}
}

func TestAdoptionHoldsIntegrationLatePreparationAndDispatch(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, stage := range []string{"prepared", "admitted"} {
			t.Run(channel+"/"+stage, func(t *testing.T) {
				f := newAttemptFixture(t)
				f.configure(t, attemptProject, "test-capability", 0, false)
				a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "late-hold-operation", channel, 2, 1))
				if _, err := f.e.ExecutionContext(t.Context(), attemptProject, a.OperationID); err != nil {
					t.Fatal("baseline preparation check", err)
				}
				anchor := attemptAnchor(attemptProject, 2)
				intent := ""
				if channel == "hosted" {
					intent = "synthetic-post-intent"
				}
				if stage == "admitted" {
					var err error
					a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, intent)
					if err != nil {
						t.Fatal(err)
					}
					if err := f.e.VerifyAction(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
						t.Fatal("baseline shared dispatch check", err)
					}
				}
				before := attemptState(t, f, a)
				integrationImportHold(t, f, 2)
				if attemptState(t, f, a) != before {
					t.Fatal("protective import rewrote an existing operation, intent or reservation")
				}
				if stage == "prepared" {
					_, err := f.e.ExecutionContext(t.Context(), attemptProject, a.OperationID)
					integrationSourceHeld(t, err)
					_, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, intent)
					integrationSourceHeld(t, err)
				} else {
					integrationSourceHeld(t, f.e.VerifyAction(t.Context(), attemptProject, a.ID, a.Revision))
				}
				_, err := f.e.RoundAdmission(t.Context(), attemptProject, a.OperationID)
				integrationSourceHeld(t, err)
				if attemptState(t, f, a) != before {
					t.Fatal("late-hold refusal mutated retained work")
				}
			})
		}
	}
}

func integrationSelectionWorker(t *testing.T, f *attemptFixture) {
	t.Helper()
	if _, err := f.store.DB().Exec(`INSERT OR IGNORE INTO workers(project_id,id,name) VALUES(?,'selection-worker','Synthetic selector')`, attemptProject); err != nil {
		t.Fatal(err)
	}
}

func TestAdoptionHoldsIntegrationQueueSkipsCannotClearHolds(t *testing.T) {
	for _, mode := range []string{"closed", "tapered", "covered", "merged"} {
		t.Run(mode, func(t *testing.T) {
			var f *attemptFixture
			reader := &refreshTestReader{}
			switch mode {
			case "covered":
				var a Attempt
				var id string
				f, a, id, _ = coverageFixture(t, 0, false)
				coverageFinalize(t, f, a, id, 0)
			case "tapered":
				f = adoptionFixture(t)
				p := adoptionPrepared()
				p.Tapered, p.InitialDry = true, 3
				adoptRound(t, f, p, "tapered-predecessor")
			case "closed":
				f = selectionFixture(t)
				zero := int64(0)
				if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "closed-predecessor", 2, "cli", RoundControlRequest{ExpectedRevision: &zero, Action: "close"}); err != nil {
					t.Fatal(err)
				}
			case "merged":
				f = selectionFixture(t)
				reader.pull = func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
					p := reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}
					if number == 2 {
						p.State, p.Merged = "closed", true
					}
					return p, nil
				}
			}
			integrationSelectionWorker(t, f)
			baseline, _ := selectionEnqueue(t, f, "baseline-skip", "cli", nil)
			selectionNext(t, f, reader)
			selected := selectionRead(t, f, baseline.ID)
			if selected.State != "selected" || roundOperation(t, f, selected.OperationID).PullNumber != 4 {
				t.Fatal("fixture did not establish a skipped predecessor", selected)
			}
			// Settle the synthetic baseline before asking for a fresh selection;
			// an unrelated inflight operation would independently stop selection.
			baselineAttempt := f.start(t, attemptProject, Receipt{ID: selected.OperationID})
			if _, err := f.e.Advance(t.Context(), attemptProject, baselineAttempt.ID, baselineAttempt.Revision, "cancelled", "synthetic-baseline-settled", ""); err != nil {
				t.Fatal(err)
			}
			integrationImportHold(t, f, 2)
			before := attemptCount(t, f.store, `SELECT count(*) FROM review_operations`)
			r, _ := selectionEnqueue(t, f, "held-skip", "cli", nil)
			selectionNext(t, f, reader)
			got := selectionRead(t, f, r.ID)
			if got.State != "held" || got.Reason != "source_obligations_pending" || got.OperationID != "" || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != before {
				t.Fatal("queue skip bypassed carried hold", got)
			}
		})
	}
}

func TestAdoptionHoldsIntegrationFinalPredecessorRecheck(t *testing.T) {
	for _, mode := range []string{"covered", "closed", "tapered", "merged"} {
		t.Run(mode, func(t *testing.T) {
			f, prior, id, predecessor := coverageFixture(t, 0, false)
			coverageFinalize(t, f, prior, id, 0)
			if mode == "closed" {
				zero := int64(0)
				if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "close", 2, "cli", RoundControlRequest{ExpectedRevision: &zero, Action: "close"}); err != nil {
					t.Fatal(err)
				}
			} else if mode == "tapered" {
				if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,'adoption-worker','Synthetic importer')`, attemptProject); err != nil {
					t.Fatal(err)
				}
				p := adoptionPrepared()
				p.Tapered, p.InitialDry = true, 3
				adoptRound(t, f, p, "taper")
			} else if mode == "merged" {
				predecessor.Pull.State, predecessor.Pull.Merged = "closed", true
			}
			previous := int64(1)
			if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "stack", Configuration{ExpectedRevision: &previous, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}}); err != nil {
				t.Fatal(err)
			}
			child := f.start(t, attemptProject, f.enqueue(t, attemptProject, "child", "cli", 4, 2))
			anchor := attemptAnchor(attemptProject, 4)
			var err error
			child, err = f.e.Admit(t.Context(), attemptProject, child.ID, child.Revision, anchor, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.e.VerifyAction(t.Context(), attemptProject, child.ID, child.Revision, predecessor); err != nil {
				t.Fatal("baseline predecessor failed", err)
			}
			before := attemptState(t, f, child)
			integrationImportHold(t, f, 2)
			integrationSourceHeld(t, f.e.VerifyAction(t.Context(), attemptProject, child.ID, child.Revision, predecessor))
			if attemptState(t, f, child) != before {
				t.Fatal("predecessor hold rewrote child work")
			}
		})
	}
}

func TestAdoptionHoldsIntegrationCaptureFinalizationPolicyAndResolution(t *testing.T) {
	f, a, input := evidenceFixture(t, "cli", false)
	input.ExpectedAnchor.ParentSHA = input.ExpectedAnchor.BaseSHA
	var err error
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, input.ExpectedAnchor, "")
	if err != nil {
		t.Fatal(err)
	}
	assessmentPolicy(t, f, 1, []string{"cli"})
	input.Review.Findings, input.Review.ReportedFindings = []reviewprovider.ParsedFinding{}, 0
	before := attemptState(t, f, a)
	request, originalReceipt, hold := integrationImportHold(t, f, 2)
	if attemptState(t, f, a) != before || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
		t.Fatal("import changed existing attempt or fabricated capture")
	}
	// A result of already admitted work remains consumable after the hold.
	evidence, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, input)
	if err != nil {
		t.Fatal("hold prevented consumption of an existing result", err)
	}
	coverageFinalize(t, f, a, evidence.ID, 0)
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	assessment, err := f.e.ReadPolicyAssessment(t.Context(), scope, evidence.ID)
	if err != nil || assessment.State != "held" || assessment.Reason != "source_obligations_pending" || assessment.SourceObligationsPending != 1 || len(assessment.Channels) != 1 || !assessment.Channels[0].Covered || assessment.Channels[0].Pending {
		t.Fatal("policy lost independent carried hold or actual captured credit", assessment, err)
	}
	plan := SourcePlan{Repository: reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, Pull: reviewprovider.Pull{Number: 2, RepositoryID: 801, State: "open", HeadSHA: input.ExpectedAnchor.CandidateSHA, BaseSHA: input.ExpectedAnchor.BaseSHA, BaseRef: "main", ChangedFiles: 1}, ParentSHA: input.ExpectedAnchor.ParentSHA, ObservedAt: stamp()}
	_, err = f.e.Comparison(t.Context(), attemptProject, plan, "cli")
	integrationSourceHeld(t, err)
	resolve := holdmodel.ResolveRequest{ExpectedRevision: &hold.Revision, Reason: "Explicit synthetic correction established", Evidence: []string{"proof:synthetic-correction"}}
	path := integrationHoldsPath + "/" + hold.ID + "/resolve"
	for _, token := range []string{f.reader} {
		w := integrationHoldRequest(t, f, "POST", path, token, "unauthorized-resolution", resolve)
		if w.Code != http.StatusForbidden {
			t.Fatalf("ordinary credential resolved hold: %d %s", w.Code, w.Body.String())
		}
	}
	stale := hold.Revision + 1
	bad := resolve
	bad.ExpectedRevision = &stale
	if w := integrationHoldRequest(t, f, "POST", path, f.admin, "stale-resolution", bad); w.Code != http.StatusConflict {
		t.Fatalf("stale resolution: %d %s", w.Code, w.Body.String())
	}
	counts := fmt.Sprint(attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`), "/", attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions`))
	if w := integrationHoldRequest(t, f, "POST", path, f.admin, "resolve-hold", resolve); w.Code != http.StatusOK {
		t.Fatalf("resolution: %d %s", w.Code, w.Body.String())
	}
	if w := integrationHoldRequest(t, f, "POST", integrationHoldsPath+"/apply", f.admin, "holds-import", request); w.Code != http.StatusOK || !bytes.Equal(originalReceipt, w.Body.Bytes()) {
		t.Fatalf("original-key replay changed after resolution: %d %s", w.Code, w.Body.String())
	}
	assessment, err = f.e.ReadPolicyAssessment(t.Context(), scope, evidence.ID)
	if err != nil || assessment.State != "sufficient" || assessment.SourceObligationsPending != 0 || assessment.Reason != "" || !assessment.Channels[0].Covered {
		t.Fatal("resolution did not release only the independent hold", assessment, err)
	}
	if counts != fmt.Sprint(attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`), "/", attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions`)) {
		t.Fatal("resolution/replay manufactured capture or completion credit")
	}
	w := integrationHoldRequest(t, f, "GET", integrationHoldsPath+"/"+hold.ID, f.reader, "", nil)
	var detail holdmodel.Detail
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Status != "resolved" || detail.Resolution == nil || !reflect.DeepEqual(detail.Original, request.Batch.Obligations[0]) {
		t.Fatal("resolution/replay lost immutable original or resolution history", w.Body.String())
	}
}

func TestAdoptionHoldsIntegrationSelectionRechecksSkippedPrefix(t *testing.T) {
	f := selectionFixture(t)
	zero := int64(0)
	if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "close-prefix", 2, "cli", RoundControlRequest{ExpectedRevision: &zero, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	imported := false
	reader := &refreshTestReader{pull: func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
		// The first prefix check has already skipped PR2. A hold arrives while
		// the synthetic source read for its successor is in progress.
		if number == 4 && !imported {
			integrationImportHold(t, f, 2)
			imported = true
		}
		return reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}
	r, _ := selectionEnqueue(t, f, "late-prefix-hold", "cli", nil)
	selectionNext(t, f, reader)
	got := selectionRead(t, f, r.ID)
	if !imported || got.State != "held" || got.Reason != "source_obligations_pending" || got.OperationID != "" || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("late prefix hold escaped transactional child admission", got)
	}
}

func TestAdoptionHoldsIntegrationSourceAssessmentsRemainProviderFree(t *testing.T) {
	f, a, _, reader, _ := readinessFixture(t)
	scope := sourceCIScope(t, f)
	baseline, err := f.e.ReadSourceReadiness(t.Context(), scope, 2)
	if err != nil || baseline.State != "ready_as_observed" {
		t.Fatal("baseline readiness", baseline, err)
	}
	integrationImportHold(t, f, 2)
	before := readinessDurable(t, f, a)
	calls, ciCalls, reviewCalls := reader.calls.Load(), reader.ciCalls.Load(), reader.reviewCalls.Load()
	policy, err := f.e.ReadSourcePolicyAssessment(t.Context(), scope, 2)
	if err != nil || policy.State != "held" || policy.Reason != "source_obligations_pending" || policy.SourceObligationsPending != 1 || len(policy.Channels) != 1 || !policy.Channels[0].Covered || policy.Channels[0].Pending {
		t.Fatal("source policy bypassed carried obligation", policy, err)
	}
	readiness, err := f.e.ReadSourceReadiness(t.Context(), scope, 2)
	if err != nil || readiness.State != "held" || readiness.SourceObligationsPending != 1 || !slices.Equal(readiness.Reasons, []string{"source_obligations_pending"}) || len(readiness.Channels) != 1 || !readiness.Channels[0].Covered {
		t.Fatal("readiness bypassed carried obligation", readiness, err)
	}
	if readinessDurable(t, f, a) != before || reader.calls.Load() != calls || reader.ciCalls.Load() != ciCalls || reader.reviewCalls.Load() != reviewCalls {
		t.Fatal("local carried-obligation assessment mutated facts or called provider")
	}
}

func TestAdoptionHoldsIntegrationSupervisorSettlesHoldArrivingDuringPrepare(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			request := Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: channel, HeadSHA: strings.Repeat("a", 40)}
			scope := f.scope(t, attemptProject, f.writer, false)
			original, err := f.e.Enqueue(t.Context(), scope, "prepare-race-request", request)
			if err != nil {
				t.Fatal(err)
			}
			var receipt Receipt
			if err := json.Unmarshal(original, &receipt); err != nil {
				t.Fatal(err)
			}
			var preparing Attempt
			driver := &controlledDriver{prepare: func(_ context.Context, o Operation, _ Configuration, a Attempt) (Preparation, error) {
				preparing = a
				if a.State != "preparing" || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 1 {
					t.Fatal("fixture did not reach reserved preparation")
				}
				before := attemptState(t, f, a)
				integrationImportHold(t, f, o.PullNumber)
				if attemptState(t, f, a) != before {
					t.Fatal("hold import rewrote preparation")
				}
				intent := ""
				if channel == "hosted" {
					intent = "synthetic-unposted-intent"
				}
				return Preparation{Anchor: attemptAnchor(o.ProjectID, o.PullNumber), Intent: intent}, nil
			}}
			drivers := map[string]Driver{"test-capability": driver}
			// processNext is the supervisor's actual single scheduling pass. The
			// HTTP import occurs after Prepare has begun and before real Admit.
			if err := f.e.processNext(t.Context(), drivers); err != nil {
				t.Fatal("late policy hold was treated as a persistence failure", err)
			}
			op := roundOperation(t, f, receipt.ID)
			if preparing.ID == "" || op.State != "held" || op.Reason != "source_obligations_pending" || op.AttemptID != preparing.ID || op.Revision != 3 {
				t.Fatal("unposted work was not settled as held", op)
			}
			var state, intent, external string
			var revision int64
			if err := f.store.DB().QueryRow(`SELECT state,revision,intent,external_id FROM review_execution_attempts WHERE id=?`, preparing.ID).Scan(&state, &revision, &intent, &external); err != nil {
				t.Fatal(err)
			}
			if state != "held" || revision != 2 || intent != "" || external != "" || driver.prepares.Load() != 1 || driver.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("late hold dispatched, retained a reservation or recorded an unposted intent", state, revision, intent, external)
			}
			settled := attemptState(t, f, preparing)
			for range 2 {
				if err := f.e.processNext(t.Context(), drivers); err != nil {
					t.Fatal("repeat scheduling failed", err)
				}
				replay, err := f.e.Enqueue(t.Context(), scope, "prepare-race-request", request)
				if err != nil || !bytes.Equal(original, replay) || attemptState(t, f, preparing) != settled {
					t.Fatal("original retry changed held work", err)
				}
			}
			_, err = f.e.Enqueue(t.Context(), scope, "new-request-still-held", request)
			integrationSourceHeld(t, err)
			if driver.prepares.Load() != 1 || driver.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions`) != 2 {
				t.Fatal("retry duplicated preparation, transition or provider post")
			}
			// Releasing the unused reservation leaves an unrelated target usable.
			f.enqueue(t, attemptProject, "unrelated-after-held-prepare", channel, 4, 1)
			other := &controlledDriver{dispatch: func(_ context.Context, _ Operation, _ Attempt, _ func(context.Context, string) error) (Result, error) {
				return Result{State: "failed", Reason: "synthetic_no_post"}, nil
			}}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": other}); err != nil || other.prepares.Load() != 1 || other.calls.Load() != 1 {
				t.Fatal("unused reservation blocked unrelated work", err)
			}
			if attemptState(t, f, preparing) != settled {
				t.Fatal("unrelated work changed the held attempt")
			}
		})
	}
}

func TestAdoptionHoldsIntegrationSupervisorRetainsPossiblyDispatchedWork(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			receipt := f.enqueue(t, attemptProject, "dispatched-before-hold", channel, 2, 1)
			var dispatched Attempt
			driver := &controlledDriver{dispatch: func(ctx context.Context, _ Operation, a Attempt, trigger func(context.Context, string) error) (Result, error) {
				dispatched = a
				if channel == "hosted" {
					if err := trigger(ctx, "synthetic-posted-trigger"); err != nil {
						t.Fatal(err)
					}
				}
				before := attemptState(t, f, a)
				integrationImportHold(t, f, 2)
				if attemptState(t, f, a) != before {
					t.Fatal("hold import rewrote possibly dispatched work")
				}
				return Result{}, sourceHold("source_obligations_pending")
			}}
			drivers := map[string]Driver{"test-capability": driver}
			if err := f.e.processNext(t.Context(), drivers); err != nil {
				t.Fatal(err)
			}
			op := roundOperation(t, f, receipt.ID)
			var state, external string
			if err := f.store.DB().QueryRow(`SELECT state,external_id FROM review_execution_attempts WHERE id=?`, dispatched.ID).Scan(&state, &external); err != nil {
				t.Fatal(err)
			}
			if op.State != "uncertain" || op.Reason != "dispatch_interrupted" || state != "uncertain" || channel == "hosted" && external != "synthetic-posted-trigger" || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, dispatched.ID) != 1 {
				t.Fatal("late dispatch hold erased uncertainty, external identity or reservation", op, state, external)
			}
			before := attemptState(t, f, dispatched)
			if err := f.e.processNext(t.Context(), drivers); err != nil || driver.prepares.Load() != 1 || driver.calls.Load() != 1 || attemptState(t, f, dispatched) != before {
				t.Fatal("repeat scheduling retried possibly dispatched work", err)
			}
		})
	}
}
