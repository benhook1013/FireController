package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func routeProjectionFixture(t *testing.T, published bool) (*attemptFixture, Attempt, ReviewEvidence, SourcePlan) {
	t.Helper()
	f, a, id, plan := coverageFixture(t, 2, false)
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
	adjudicationComplete(t, f, a)
	evidence, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
	if err != nil {
		t.Fatal(err)
	}
	if published {
		merged := projectionPull(1)
		merged.State = "merged"
		historical := projectionRun(evidence)
		historical.Channel = "manual"
		merged.Runs = []reviews.RunInput{historical}
		source := projectionPull(2)
		source.Title = "Current stored source facts"
		source.HeadSHA = strings.Repeat("9", 40)
		source.BaseSHA = strings.Repeat("8", 40)
		source.CIState = "pending"
		request := reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: evidence.Anchor.RepositoryID, Source: "route-import", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{merged, source, projectionPull(4)}}
		w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "route-projection-import", encoded(request))
		if w.Code != 201 {
			t.Fatal(w.Code, w.Body.String())
		}
		if _, err = f.e.PublishEvidence(t.Context(), attemptProject, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	return f, a, evidence, plan
}

func createProjectionRoute(t *testing.T, f *attemptFixture, id, key string, revision, finding int64) findingRouteReceipt {
	t.Helper()
	body, err := f.e.CreateFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), key, id, FindingRouteRequest{ExpectedRevision: adjudicationRevision(revision), Finding: finding, TargetPullNumber: 4, Reason: "Original routing reason <literal>"})
	var receipt findingRouteReceipt
	if err != nil || json.Unmarshal(body, &receipt) != nil || receipt.State != "routed" || receipt.SourceRevision != revision+1 {
		t.Fatal("create route", string(body), err)
	}
	return receipt
}
func routeProjectionRead(t *testing.T, f *attemptFixture, evidenceID string) reviews.RunDetailResponse {
	t.Helper()
	var runs reviews.RunsResponse
	projectionRead(t, f, "/pulls/2/runs", &runs)
	for _, run := range runs.Items {
		if run.Source == "native-executor" && run.SourceRunID == evidenceID {
			var detail reviews.RunDetailResponse
			projectionRead(t, f, "/runs/"+run.ID, &detail)
			return detail
		}
	}
	t.Fatal("native source run missing")
	return reviews.RunDetailResponse{}
}
func routeProjectionDurableState(t *testing.T, f *attemptFixture, a Attempt) string {
	t.Helper()
	var routes, history string
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(body_json,''),'') FROM review_finding_routes`).Scan(&routes); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(body_json||actor_id||kind,''),'') FROM review_finding_route_revisions`).Scan(&history); err != nil {
		t.Fatal(err)
	}
	return adjudicationState(t, f, a) + projectionDigest(t, f, "") + routes + history
}

func TestRouteProjectionReceiverStatePreservesSourceAndCoverage(t *testing.T) {
	f, a, evidence, plan := routeProjectionFixture(t, true)
	original := routeProjectionRead(t, f, evidence.ID)
	oldDigest := projectionDigest(t, f, original.SnapshotID)
	var captureBefore string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE id=?`, evidence.ID).Scan(&captureBefore); err != nil {
		t.Fatal(err)
	}
	var mergedBefore reviews.PullResponse
	projectionRead(t, f, "/pulls/1", &mergedBefore)
	route := createProjectionRoute(t, f, evidence.ID, "route-source-finding", 0, 0)
	adjudicationDecide(t, f, evidence.ID, "reject-local-finding", 1, 1, "rejected")
	sourceBefore := adjudicationRead(t, f, evidence.ID)
	// Receiving rejection before source finalization must not alter source state.
	body, err := f.e.DecideFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), "receiver-reject", route.ID, FindingRouteDecisionRequest{ExpectedRevision: adjudicationRevision(1), Disposition: "rejected", Reason: "Receiving issue does not apply"})
	adjudicationReceipt(t, body, err, route.ID, "adjudicated", 2)
	if !reflect.DeepEqual(sourceBefore, adjudicationRead(t, f, evidence.ID)) {
		t.Fatal("receiver rewrote source attribution/revision")
	}
	coverageRead(t, f, a, plan, false)
	body, err = f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "route-finalize", evidence.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(2)})
	adjudicationReceipt(t, body, err, evidence.ID, "finalized", 3)
	frozenSource := adjudicationRead(t, f, evidence.ID)
	if frozenSource.FrozenCounts == nil || *frozenSource.FrozenCounts != (FindingCounts{Found: 2, Routed: 1}) {
		t.Fatal("routed source count not frozen", frozenSource)
	}
	coverageRead(t, f, a, plan, true)
	rejected := routeProjectionRead(t, f, evidence.ID)
	if rejected.Findings[0].Route == nil || rejected.Findings[0].Route.Disposition != "rejected" || rejected.Run.Fixed != 0 || rejected.Run.Routed != 1 {
		t.Fatal("receiver rejection confused with fixed source", rejected)
	}
	rejectedDigest := projectionDigest(t, f, rejected.SnapshotID)
	body, err = f.e.DecideFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), "receiver-correct-accept", route.ID, FindingRouteDecisionRequest{ExpectedRevision: adjudicationRevision(2), Disposition: "accepted", Reason: "New receiving evidence requires a fix"})
	adjudicationReceipt(t, body, err, route.ID, "adjudicated", 3)
	coverageRead(t, f, a, plan, false)
	fix := FindingRouteFixRequest{ExpectedRevision: adjudicationRevision(3), ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("d", 40), ProofNote: "Receiving proof <script>literal</script>"}
	body, err = f.e.RecordFindingRouteFix(t.Context(), f.scope(t, attemptProject, f.writer, false), "receiver-fix", route.ID, fix)
	adjudicationReceipt(t, body, err, route.ID, "fix_recorded", 4)
	coverageRead(t, f, a, plan, true)
	fixed := routeProjectionRead(t, f, evidence.ID)
	if fixed.Run.Fixed != 0 || fixed.Run.Accepted != 0 || fixed.Run.Routed != 1 || fixed.Findings[0].FixSHA != "" || fixed.Findings[0].Route.FixSHA != fix.FixSHA || fixed.Findings[0].Actor != frozenSource.Findings[0].ActorID || fixed.Findings[0].DecisionReason != frozenSource.Findings[0].Reason {
		t.Fatal("nested receiving fix rewrote source facts", fixed)
	}
	correction := FindingRouteFixRequest{ExpectedRevision: adjudicationRevision(4), ExpectedSHA: &fix.FixSHA, FixSHA: strings.Repeat("e", 40), ProofNote: "Corrected receiving proof"}
	body, err = f.e.RecordFindingRouteFix(t.Context(), f.scope(t, attemptProject, f.writer, false), "receiver-fix-correction", route.ID, correction)
	adjudicationReceipt(t, body, err, route.ID, "fix_recorded", 5)
	if !reflect.DeepEqual(frozenSource, adjudicationRead(t, f, evidence.ID)) {
		t.Fatal("receiver proof correction rewrote frozen counts/source actor")
	}
	beforeReplay := routeProjectionDurableState(t, f, a)
	replay, err := f.e.CreateFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), "route-source-finding", evidence.ID, FindingRouteRequest{ExpectedRevision: adjudicationRevision(0), Finding: 0, TargetPullNumber: 4, Reason: "Original routing reason <literal>"})
	var originalReceipt findingRouteReceipt
	if err != nil || json.Unmarshal(replay, &originalReceipt) != nil || originalReceipt != route || beforeReplay != routeProjectionDurableState(t, f, a) {
		t.Fatal("original route replay changed after receiver advancement", string(replay), err)
	}
	// Whole-run publication replay includes the nested route DTO, with no snapshot.
	if _, err = f.e.PublishEvidence(t.Context(), attemptProject, evidence.ID, nil); err != nil || beforeReplay != routeProjectionDurableState(t, f, a) {
		t.Fatal("publication replay lost current receiving proof", err)
	}
	var captureAfter string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE id=?`, evidence.ID).Scan(&captureAfter); err != nil {
		t.Fatal(err)
	}
	var mergedAfter reviews.PullResponse
	projectionRead(t, f, "/pulls/1", &mergedAfter)
	if captureAfter != captureBefore || oldDigest != projectionDigest(t, f, original.SnapshotID) || rejectedDigest != projectionDigest(t, f, rejected.SnapshotID) || !reflect.DeepEqual(mergedBefore.Pull, mergedAfter.Pull) {
		t.Fatal("route changes rewrote capture/old snapshots/merged history")
	}
	var source reviews.PullResponse
	projectionRead(t, f, "/pulls/2", &source)
	if source.Pull.Title != "Current stored source facts" || source.Pull.HeadSHA != strings.Repeat("9", 40) || source.Pull.CIState != "pending" {
		t.Fatal("routing replaced current PR metadata", source)
	}
}

func TestRouteProjectionReceiverFailureRollsBackWholeMutation(t *testing.T) {
	f, a, evidence, _ := routeProjectionFixture(t, true)
	route := createProjectionRoute(t, f, evidence.ID, "route-before-fault", 0, 0)
	var snapshot string
	if err := f.store.DB().QueryRow(`SELECT snapshot_id FROM review_workspace WHERE project_id=?`, attemptProject).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	// A genuine projection identity conflict occurs after the receiving ledger
	// write in the same transaction; every staged effect must roll back.
	if _, err := f.store.DB().Exec(`UPDATE review_findings SET source_finding_id='synthetic-wrong-identity' WHERE project_id=? AND snapshot_id=? AND position=0 AND run_id IN (SELECT id FROM review_runs WHERE project_id=? AND snapshot_id=? AND source='native-executor')`, attemptProject, snapshot, attemptProject, snapshot); err != nil {
		t.Fatal(err)
	}
	before := routeProjectionDurableState(t, f, a)
	_, err := f.e.DecideFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), "rollback-receiver", route.ID, FindingRouteDecisionRequest{ExpectedRevision: adjudicationRevision(1), Disposition: "accepted", Reason: "Would change receiver"})
	attemptCode(t, err, "observation_conflict")
	if before != routeProjectionDurableState(t, f, a) {
		t.Fatal("projection fault left route revision/history/event/receipt")
	}
}

func TestRouteProjectionLedgerOnlyInitialPublicationUsesLatestReceiverProof(t *testing.T) {
	f, a, evidence, plan := routeProjectionFixture(t, false)
	route := createProjectionRoute(t, f, evidence.ID, "ledger-only-route", 0, 0)
	adjudicationDecide(t, f, evidence.ID, "ledger-only-local", 1, 1, "rejected")
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "ledger-only-finalize", evidence.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(2)})
	adjudicationReceipt(t, body, err, evidence.ID, "finalized", 3)
	coverageRead(t, f, a, plan, false)
	body, err = f.e.DecideFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), "ledger-only-reject", route.ID, FindingRouteDecisionRequest{ExpectedRevision: adjudicationRevision(1), Disposition: "rejected", Reason: "Resolved at receiving PR"})
	adjudicationReceipt(t, body, err, route.ID, "adjudicated", 2)
	coverageRead(t, f, a, plan, true)
	var snapshots int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_snapshots`).Scan(&snapshots); err != nil || snapshots != 0 {
		t.Fatal("ledger-only route invented browser run", snapshots, err)
	}
	pull := projectionPull(2)
	if _, err = f.e.PublishEvidence(t.Context(), attemptProject, evidence.ID, &pull); err != nil {
		t.Fatal(err)
	}
	detail := routeProjectionRead(t, f, evidence.ID)
	if detail.Findings[0].Route == nil || detail.Findings[0].Route.Disposition != "rejected" || detail.Run.Routed != 1 || !detail.Run.Finalized {
		t.Fatal("initial publication lost preexisting receiver state", detail)
	}
	// Corrupt a valid-looking compact row; exact replay must compare route too.
	if _, err = f.store.DB().Exec(`UPDATE review_findings SET route_json=replace(route_json,'Resolved at receiving PR','Changed compact route') WHERE project_id=? AND snapshot_id=?`, attemptProject, detail.SnapshotID); err != nil {
		t.Fatal(err)
	}
	_, err = f.e.PublishEvidence(t.Context(), attemptProject, evidence.ID, nil)
	attemptCode(t, err, "observation_conflict")
}

// Keep the scoped route lookup within the same caller-owned SQL snapshot.
func TestRouteProjectionRejectsCorruptActiveSourceBinding(t *testing.T) {
	f, _, evidence, plan := routeProjectionFixture(t, true)
	route := createProjectionRoute(t, f, evidence.ID, "route-binding", 0, 0)
	adjudicationDecide(t, f, evidence.ID, "binding-local", 1, 1, "rejected")
	_, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "binding-finalize", evidence.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(2)})
	if err != nil {
		t.Fatal(err)
	}
	// An internally consistent route ledger whose original source was replaced
	// still cannot become proof for another source evidence/finding.
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var routeState ReviewRoute
		routeState, err = readFindingRoute(ctx, tx, attemptProject, route.ID)
		if err != nil {
			return err
		}
		routeState.SourceHeadSHA = strings.Repeat("f", 40)
		if _, err = tx.Exec(`UPDATE review_finding_routes SET source_head_sha=?,body_json=? WHERE id=?`, routeState.SourceHeadSHA, encoded(routeState), route.ID); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE review_finding_route_revisions SET body_json=? WHERE route_id=?`, encoded(routeState), route.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.e.Comparison(t.Context(), attemptProject, plan, "cli")
	if err == nil {
		t.Fatal("corrupt source binding supplied a route obligation")
	}
}

func TestRouteProjectionImportedRoutedHistoryCannotSupplyNativeCredit(t *testing.T) {
	f := newAttemptFixture(t)
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	reviews.Register(mux, f.store)
	f.handler = mux
	pull := projectionPull(2)
	pull.Runs = []reviews.RunInput{{SourceRunID: "imported-routed-source", Channel: "cli", HeadSHA: pull.HeadSHA, Reviewer: "Imported reviewer", Scope: "Historical comparison", Outcome: "completed", Attributable: true, Complete: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", Found: 1, Routed: 1, Finalized: true, Findings: []reviews.Finding{{SourceFindingID: "legacy-route", Title: "Imported routed finding", Detail: "Retained historical observation", Severity: "low", Disposition: "routed", DecisionReason: "Historical assignment", Actor: "historical-actor", DecidedAt: "2026-01-01T00:02:00Z"}}}}
	request := reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: 801, Source: "route-import", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{pull}}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "import-routed-only", encoded(request))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	plan := SourcePlan{Repository: reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, Pull: reviewprovider.Pull{RepositoryID: 801, Number: 2, State: "open", HeadSHA: pull.HeadSHA, BaseSHA: pull.BaseSHA, BaseRef: "main", ChangedFiles: 1}, ParentSHA: pull.BaseSHA, ObservedAt: stamp()}
	for _, channel := range []string{"cli", "hosted"} {
		state, err := f.e.Comparison(t.Context(), attemptProject, plan, channel)
		if err != nil || state.Covered || state.Pending {
			t.Fatal("imported routing invented native proof/obligations", channel, state, err)
		}
	}
}
