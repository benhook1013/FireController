package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func routeFixture(t *testing.T, n int) (*attemptFixture, Attempt, string) {
	t.Helper()
	f, a, id := adjudicationFixture(t, n, false)
	previous := int64(1)
	_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "route-queue", Configuration{ExpectedRevision: &previous, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4, 6}, Channels: []string{"cli", "hosted"}})
	if err != nil {
		t.Fatal(err)
	}
	return f, a, id
}
func routeRequest(revision, finding, target int64) FindingRouteRequest {
	return FindingRouteRequest{ExpectedRevision: adjudicationRevision(revision), Finding: finding, TargetPullNumber: target, Reason: "Route exact source <script>literal</script>"}
}
func routeCreate(t *testing.T, f *attemptFixture, evidence, key string, request FindingRouteRequest) findingRouteReceipt {
	t.Helper()
	raw, err := f.e.CreateFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), key, evidence, request)
	var r findingRouteReceipt
	var shape map[string]json.RawMessage
	if err != nil || json.Unmarshal(raw, &r) != nil || json.Unmarshal(raw, &shape) != nil || len(shape) != 4 || !validFindingRouteID(r.ID) || r.State != "routed" || r.Revision != 1 || r.SourceRevision != *request.ExpectedRevision+1 {
		t.Fatalf("route receipt %s %v", raw, err)
	}
	return r
}
func routeRead(t *testing.T, f *attemptFixture, id string) FindingRouteDetail {
	t.Helper()
	r, err := f.e.ReadFindingRoute(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func routeDecision(t *testing.T, f *attemptFixture, id, key string, revision int64, disposition string) {
	t.Helper()
	raw, err := f.e.DecideFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), key, id, FindingRouteDecisionRequest{ExpectedRevision: adjudicationRevision(revision), Disposition: disposition, Reason: "Explicit receiving reason"})
	adjudicationReceipt(t, raw, err, id, "adjudicated", revision+1)
}
func routeFix(t *testing.T, f *attemptFixture, id, key string, revision int64, prior, sha string) {
	t.Helper()
	raw, err := f.e.RecordFindingRouteFix(t.Context(), f.scope(t, attemptProject, f.writer, false), key, id, FindingRouteFixRequest{ExpectedRevision: adjudicationRevision(revision), ExpectedSHA: &prior, FixSHA: sha, ProofNote: "Recorded focused proof"})
	adjudicationReceipt(t, raw, err, id, "fix_recorded", revision+1)
}
func routeState(t *testing.T, f *attemptFixture, a Attempt) string {
	t.Helper()
	var rows, history, facts, invalidations string
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(id||revision||body_json,''),'') FROM review_finding_routes`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(route_id||revision||actor_id||kind||created_at||body_json,''),'') FROM review_finding_route_revisions`).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(record,''),'') FROM (SELECT route_id||project_id||target_pull_number||revision||active||pending AS record FROM review_route_policy_facts ORDER BY route_id)`).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(record,''),'') FROM (SELECT project_id||target_pull_number||route_id AS record FROM review_route_policy_invalidations ORDER BY project_id,target_pull_number,route_id)`).Scan(&invalidations); err != nil {
		t.Fatal(err)
	}
	return adjudicationState(t, f, a) + policyState(t, f) + rows + history + facts + invalidations
}
func routePending(t *testing.T, f *attemptFixture, target int64) (int64, error) {
	t.Helper()
	var count int64
	err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		count, err = pendingIncomingRoutes(ctx, tx, attemptProject, target)
		return err
	})
	return count, err
}

func TestFindingRouteCreationReplayReceiverProofAndFrozenCounts(t *testing.T) {
	f, a, id := routeFixture(t, 2)
	original, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
	if err != nil {
		t.Fatal(err)
	}
	request := routeRequest(0, 0, 4)
	created := routeCreate(t, f, id, "route-create", request)
	state := adjudicationRead(t, f, id)
	if state.Revision != 1 || state.Findings[0].Disposition != "routed" || state.Findings[0].RouteID != created.ID {
		t.Fatal(state)
	}
	detail := routeRead(t, f, created.ID)
	if detail.Route.RoutingActorID != state.Findings[0].ActorID || detail.Route.CreatedAt != state.Findings[0].DecidedAt || detail.SourceAnchor != original.Anchor || !reflect.DeepEqual(detail.SourceFinding, original.Capture.Review.Findings[0]) {
		t.Fatal("origin lost", detail)
	}
	before := routeState(t, f, a)
	raw, err := f.e.CreateFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), "route-create", id, request)
	if err != nil || string(raw) != encoded(created) || routeState(t, f, a) != before {
		t.Fatal("route replay wrote state", string(raw), err)
	}
	if count, err := routePending(t, f, 4); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	adjudicationDecide(t, f, id, "reject-other", 1, 1, "rejected")
	adjudicationComplete(t, f, a)
	raw, err = f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "source-finalize", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(2)})
	adjudicationReceipt(t, raw, err, id, "finalized", 3)
	sourceBefore := adjudicationRead(t, f, id)
	if sourceBefore.FrozenCounts == nil || *sourceBefore.FrozenCounts != (FindingCounts{Found: 2, Routed: 1}) {
		t.Fatal(sourceBefore)
	}
	routeDecision(t, f, created.ID, "receive-accept", 1, "accepted")
	if count, err := routePending(t, f, 4); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	routeFix(t, f, created.ID, "receive-fix", 2, "", strings.Repeat("a", 40))
	if count, err := routePending(t, f, 4); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	routeFix(t, f, created.ID, "receive-correct-fix", 3, strings.Repeat("a", 40), strings.Repeat("b", 40))
	after := adjudicationRead(t, f, id)
	if !reflect.DeepEqual(sourceBefore, after) {
		t.Fatal("receiver rewrote source attribution/revision/counts", sourceBefore, after)
	}
	if got := routeRead(t, f, created.ID).Route; got.Revision != 4 || got.FixSHA != strings.Repeat("b", 40) || got.Disposition != "accepted" {
		t.Fatal(got)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_finding_route_revisions WHERE route_id=?`, created.ID) != 4 {
		t.Fatal("complete route history missing")
	}
	retained, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
	if err != nil || !reflect.DeepEqual(retained, original) {
		t.Fatal("route changed immutable capture", err)
	}
	// Exact receiver retry remains read-only after later receiving revisions.
	before = routeState(t, f, a)
	raw, err = f.e.RecordFindingRouteFix(t.Context(), f.scope(t, attemptProject, f.writer, false), "receive-fix", created.ID, FindingRouteFixRequest{ExpectedRevision: adjudicationRevision(2), ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("a", 40), ProofNote: "Recorded focused proof"})
	adjudicationReceipt(t, raw, err, created.ID, "fix_recorded", 3)
	if routeState(t, f, a) != before {
		t.Fatal("receiver replay mutated history")
	}
}

func TestFindingRouteRerouteAndSourceCorrectionRequireReceiverRevision(t *testing.T) {
	f, a, id := routeFixture(t, 1)
	first := routeCreate(t, f, id, "route-first", routeRequest(0, 0, 4))
	bad := routeRequest(1, 0, 6)
	before := routeState(t, f, a)
	_, err := f.e.CreateFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), "bad-reroute", id, bad)
	attemptCode(t, err, "review_conflict")
	if routeState(t, f, a) != before {
		t.Fatal("stale reroute wrote")
	}
	request := bad
	request.ExpectedRouteRevision = 1
	second := routeCreate(t, f, id, "route-second", request)
	if second.ID == first.ID || second.SourceRevision != 2 || routeRead(t, f, first.ID).Route.State != "superseded" || routeRead(t, f, first.ID).Route.Revision != 2 {
		t.Fatal("reroute reused/lost old identity")
	}
	if count, err := routePending(t, f, 4); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if count, err := routePending(t, f, 6); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	_, err = f.e.DecideFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), "old-receiver", first.ID, FindingRouteDecisionRequest{ExpectedRevision: adjudicationRevision(2), Disposition: "accepted", Reason: "Cannot act on superseded"})
	attemptCode(t, err, "review_conflict")
	routeDecision(t, f, second.ID, "receive-second", 1, "rejected")
	replacement := FindingDecisionRequest{ExpectedRevision: adjudicationRevision(2), Finding: 0, Disposition: "accepted", Reason: "Explicit source correction", ExpectedRouteRevision: 1}
	_, err = f.e.DecideReviewFinding(t.Context(), f.scope(t, attemptProject, f.writer, false), "stale-source-replace", id, replacement)
	attemptCode(t, err, "review_conflict")
	replacement.ExpectedRouteRevision = 2
	raw, err := f.e.DecideReviewFinding(t.Context(), f.scope(t, attemptProject, f.writer, false), "source-replace", id, replacement)
	adjudicationReceipt(t, raw, err, id, "adjudicated", 3)
	if f := adjudicationRead(t, f, id).Findings[0]; f.RouteID != "" || f.Disposition != "accepted" {
		t.Fatal("source kept old routing link", f)
	}
	if old := routeRead(t, f, second.ID).Route; old.State != "superseded" || old.Revision != 3 || old.Disposition != "rejected" {
		t.Fatal(old)
	}
}

func TestFindingRouteProtectsSourceAndReceiverFixEvidence(t *testing.T) {
	t.Run("sourcefix", func(t *testing.T) {
		f, a, id := routeFixture(t, 1)
		adjudicationDecide(t, f, id, "accept-source", 0, 0, "accepted")
		raw, err := f.e.RecordFindingFix(t.Context(), f.scope(t, attemptProject, f.writer, false), "fix-source", id, FindingFixRequest{ExpectedRevision: adjudicationRevision(1), Finding: 0, ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("c", 40), ProofNote: "Keep proof"})
		adjudicationReceipt(t, raw, err, id, "fix_recorded", 2)
		before := routeState(t, f, a)
		_, err = f.e.CreateFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), "route-proved-source", id, routeRequest(2, 0, 4))
		attemptCode(t, err, "review_conflict")
		if routeState(t, f, a) != before {
			t.Fatal("source proof lost")
		}
	})
	t.Run("receiverfix", func(t *testing.T) {
		f, a, id := routeFixture(t, 1)
		r := routeCreate(t, f, id, "route", routeRequest(0, 0, 4))
		routeDecision(t, f, r.ID, "accept", 1, "accepted")
		routeFix(t, f, r.ID, "fix", 2, "", strings.Repeat("c", 40))
		before := routeState(t, f, a)
		_, err := f.e.DecideFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), "reject-proof", r.ID, FindingRouteDecisionRequest{ExpectedRevision: adjudicationRevision(3), Disposition: "rejected", Reason: "Cannot discard proof"})
		attemptCode(t, err, "review_conflict")
		request := routeRequest(1, 0, 6)
		request.ExpectedRouteRevision = 3
		_, err = f.e.CreateFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), "reroute-proof", id, request)
		attemptCode(t, err, "review_conflict")
		_, err = f.e.DecideReviewFinding(t.Context(), f.scope(t, attemptProject, f.writer, false), "source-replace-proof", id, FindingDecisionRequest{ExpectedRevision: adjudicationRevision(1), Finding: 0, Disposition: "accepted", Reason: "Cannot discard receiving proof", ExpectedRouteRevision: 3})
		attemptCode(t, err, "review_conflict")
		_, err = f.e.RecordFindingRouteFix(t.Context(), f.scope(t, attemptProject, f.writer, false), "wrong-fix-guard", r.ID, FindingRouteFixRequest{ExpectedRevision: adjudicationRevision(3), ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("d", 40), ProofNote: "Wrong guard"})
		attemptCode(t, err, "review_conflict")
		if routeState(t, f, a) != before {
			t.Fatal("proof protection partially wrote")
		}
	})
}

func TestFindingRouteCreationAfterSourceFinalizationRetainsOriginalCounts(t *testing.T) {
	f, a, id := routeFixture(t, 1)
	adjudicationDecide(t, f, id, "source-accept", 0, 0, "accepted")
	adjudicationComplete(t, f, a)
	raw, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "finalize", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
	adjudicationReceipt(t, raw, err, id, "finalized", 2)
	r := routeCreate(t, f, id, "route-after-final", routeRequest(2, 0, 4))
	state := adjudicationRead(t, f, id)
	if !state.Finalized || *state.FrozenCounts != (FindingCounts{Found: 1, Accepted: 1}) || state.Findings[0].RouteID != r.ID {
		t.Fatal("route rewrote immutable counts", state)
	}
	routeDecision(t, f, r.ID, "reject-route", 1, "rejected")
	if *adjudicationRead(t, f, id).FrozenCounts != (FindingCounts{Found: 1, Accepted: 1}) {
		t.Fatal("receiving correction changed frozen counts")
	}
}

func TestFindingRouteConcurrentSourceAndReceivingGuards(t *testing.T) {
	f, a, id := routeFixture(t, 1)
	scope := f.scope(t, attemptProject, f.writer, false)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := range 2 {
		wg.Go(func() {
			_, err := f.e.CreateFindingRoute(t.Context(), scope, fmt.Sprintf("competing-route-%d", i), id, routeRequest(0, 0, 4))
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	success, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else {
			attemptCode(t, err, "review_conflict")
			conflicts++
		}
	}
	if success != 1 || conflicts != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_finding_routes`) != 1 {
		t.Fatal(success, conflicts)
	}
	routeID := adjudicationRead(t, f, id).Findings[0].RouteID
	errs = make(chan error, 2)
	wg.Go(func() {
		_, err := f.e.DecideFindingRoute(t.Context(), scope, "receiving-race", routeID, FindingRouteDecisionRequest{ExpectedRevision: adjudicationRevision(1), Disposition: "accepted", Reason: "Receiving race"})
		errs <- err
	})
	wg.Go(func() {
		_, err := f.e.DecideReviewFinding(t.Context(), scope, "source-race", id, FindingDecisionRequest{ExpectedRevision: adjudicationRevision(1), Finding: 0, Disposition: "accepted", Reason: "Source race", ExpectedRouteRevision: 1})
		errs <- err
	})
	wg.Wait()
	close(errs)
	success, conflicts = 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else {
			attemptCode(t, err, "review_conflict")
			conflicts++
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatal("receiving race erased state", success, conflicts)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_finding_routes WHERE state='active'`) > 1 {
		t.Fatal("multiple active source identities")
	}
	_ = a
}

func TestFindingRouteProjectionFailureRollsBackEveryEffect(t *testing.T) {
	for _, phase := range []string{"create", "receive", "fix", "reroute", "source"} {
		t.Run(phase, func(t *testing.T) {
			f, a, id := routeFixture(t, 1)
			var route findingRouteReceipt
			if phase != "create" {
				route = routeCreate(t, f, id, "initial-route", routeRequest(0, 0, 4))
			}
			if phase == "fix" {
				routeDecision(t, f, route.ID, "accept", 1, "accepted")
			}
			f.e.publishAdjudication = func(context.Context, *sql.Tx, core.Scope, ReviewAdjudication) error {
				return core.Fail("workspace_capacity", "Synthetic projection refusal.", 409)
			}
			before := routeState(t, f, a)
			var err error
			switch phase {
			case "create":
				_, err = f.e.CreateFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), "rollback", id, routeRequest(0, 0, 4))
			case "receive":
				_, err = f.e.DecideFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), "rollback", route.ID, FindingRouteDecisionRequest{ExpectedRevision: adjudicationRevision(1), Disposition: "accepted", Reason: "Receiving choice"})
			case "fix":
				_, err = f.e.RecordFindingRouteFix(t.Context(), f.scope(t, attemptProject, f.writer, false), "rollback", route.ID, FindingRouteFixRequest{ExpectedRevision: adjudicationRevision(2), ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("a", 40), ProofNote: "Proof"})
			case "reroute":
				r := routeRequest(1, 0, 6)
				r.ExpectedRouteRevision = 1
				_, err = f.e.CreateFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), "rollback", id, r)
			case "source":
				_, err = f.e.DecideReviewFinding(t.Context(), f.scope(t, attemptProject, f.writer, false), "rollback", id, FindingDecisionRequest{ExpectedRevision: adjudicationRevision(1), Finding: 0, Disposition: "rejected", Reason: "Source correction", ExpectedRouteRevision: 1})
			}
			attemptCode(t, err, "workspace_capacity")
			if routeState(t, f, a) != before {
				t.Fatal("projection failure changed ledger/history/events/receipt", phase)
			}
		})
	}
}

func TestFindingRouteScopedAuthorizationRepositoryAndNoExecutor(t *testing.T) {
	f, a, id := routeFixture(t, 1)
	scope := f.scope(t, attemptProject, f.writer, false)
	r := routeCreate(t, f, id, "route", routeRequest(0, 0, 4))
	before := routeState(t, f, a)
	_, err := f.e.DecideFindingRoute(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), "read-only", r.ID, FindingRouteDecisionRequest{ExpectedRevision: adjudicationRevision(1), Disposition: "accepted", Reason: "Unauthorized"})
	attemptCode(t, err, "forbidden")
	_, err = f.e.ReadFindingRoute(t.Context(), evidenceReadScope(t, f, otherAttemptProject, f.other), r.ID)
	attemptCode(t, err, "not_found")
	admissionSQL(t, f, `UPDATE projects SET github_repository_id=999 WHERE id=?`, attemptProject)
	_, err = f.e.DecideFindingRoute(t.Context(), scope, "repo-changed", r.ID, FindingRouteDecisionRequest{ExpectedRevision: adjudicationRevision(1), Disposition: "accepted", Reason: "Changed repository"})
	attemptCode(t, err, "review_held")
	admissionSQL(t, f, `UPDATE projects SET github_repository_id=801 WHERE id=?`, attemptProject)
	if routeState(t, f, a) != before {
		t.Fatal("refusals wrote route state")
	}
	f.e, err = New(f.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	routeDecision(t, f, r.ID, "local-without-provider", 1, "rejected")
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
	_, err = f.e.CreateFindingRoute(t.Context(), scope, "route", id, routeRequest(0, 0, 4))
	attemptCode(t, err, "unauthorized")
}

func TestFindingRoutePaginationBoundCursorAndQueryPlan(t *testing.T) {
	f, _, id := routeFixture(t, 8)
	for i := range 8 {
		target := int64(4)
		if i == 7 {
			target = 6
		}
		routeCreate(t, f, id, fmt.Sprintf("route-%d", i), routeRequest(int64(i), int64(i), target))
	}
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	first, err := f.e.ListFindingRoutes(t.Context(), scope, 4, 0, "")
	if err != nil || len(first.Items) != 5 || !first.HasMore || first.Cursor == "" {
		t.Fatal(first, err)
	}
	second, err := f.e.ListFindingRoutes(t.Context(), scope, 4, 5, first.Cursor)
	if err != nil || len(second.Items) != 2 || second.HasMore {
		t.Fatal(second, err)
	}
	seen := map[string]bool{}
	for _, item := range append(first.Items, second.Items...) {
		if seen[item.ID] || item.TargetPullNumber != 4 {
			t.Fatal("pagination duplicate/crossfilter")
		}
		seen[item.ID] = true
	}
	raw, _ := json.Marshal(first)
	if bytes.Contains(raw, []byte("Immutable finding")) || bytes.Contains(raw, []byte("routing_reason")) || bytes.Contains(raw, []byte("source_anchor")) {
		t.Fatal("list leaked full detail", string(raw))
	}
	_, err = f.e.ListFindingRoutes(t.Context(), scope, 6, 5, first.Cursor)
	attemptCode(t, err, "validation_failed")
	_, err = f.e.ListFindingRoutes(t.Context(), evidenceReadScope(t, f, otherAttemptProject, f.other), 4, 5, first.Cursor)
	attemptCode(t, err, "validation_failed")
	_, err = f.e.ListFindingRoutes(t.Context(), scope, 4, 101, "")
	attemptCode(t, err, "validation_failed")
	rows, err := f.store.DB().Query(`EXPLAIN QUERY PLAN SELECT id FROM review_finding_routes WHERE project_id=? AND target_pull_number=? AND state='active' AND (created_at,id)<(?,?) ORDER BY created_at DESC,id DESC LIMIT 6`, attemptProject, 4, first.Items[4].CreatedAt, first.Items[4].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var a, b, c int
		var d string
		if err = rows.Scan(&a, &b, &c, &d); err != nil {
			t.Fatal(err)
		}
		plan += d
	}
	if !strings.Contains(plan, "review_finding_routes_incoming") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatal("unbounded/sorted pagination", plan)
	}
}

func TestFindingRouteCorruptionAndResolvedPendingValidation(t *testing.T) {
	for _, mode := range []string{"unknown", "alias", "badindex", "badcurrenthistory", "badorigin", "missinghistory", "badtime", "resolvedfix"} {
		t.Run(mode, func(t *testing.T) {
			f, _, id := routeFixture(t, 1)
			r := routeCreate(t, f, id, "route", routeRequest(0, 0, 4))
			if mode == "resolvedfix" {
				routeDecision(t, f, r.ID, "accept", 1, "accepted")
				routeFix(t, f, r.ID, "fix", 2, "", strings.Repeat("a", 40))
			}
			var body string
			if err := f.store.DB().QueryRow(`SELECT body_json FROM review_finding_routes WHERE id=?`, r.ID).Scan(&body); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "unknown":
				body = strings.Replace(body, `"id":`, `"unknown":1,"id":`, 1)
				admissionSQL(t, f, `UPDATE review_finding_routes SET body_json=? WHERE id=?`, body, r.ID)
			case "alias":
				body = strings.Replace(body, `"state":`, `"State":`, 1)
				admissionSQL(t, f, `UPDATE review_finding_routes SET body_json=? WHERE id=?`, body, r.ID)
			case "badindex":
				admissionSQL(t, f, `UPDATE review_finding_routes SET repository_id=999 WHERE id=?`, r.ID)
			case "badcurrenthistory":
				admissionSQL(t, f, `UPDATE review_finding_route_revisions SET body_json='{}' WHERE route_id=? AND revision=1`, r.ID)
			case "badorigin":
				admissionSQL(t, f, `UPDATE review_finding_route_revisions SET actor_id='wrong-actor' WHERE route_id=? AND revision=1`, r.ID)
			case "missinghistory":
				admissionSQL(t, f, `DELETE FROM review_finding_route_revisions WHERE route_id=?`, r.ID)
			case "badtime":
				var route ReviewRoute
				if json.Unmarshal([]byte(body), &route) != nil {
					t.Fatal("fixture")
				}
				route.UpdatedAt = "not-a-time"
				admissionSQL(t, f, `UPDATE review_finding_routes SET body_json=? WHERE id=?`, encoded(route), r.ID)
				admissionSQL(t, f, `UPDATE review_finding_route_revisions SET body_json=?,created_at=? WHERE route_id=? AND revision=?`, encoded(route), route.UpdatedAt, r.ID, route.Revision)
			case "resolvedfix":
				var route ReviewRoute
				if json.Unmarshal([]byte(body), &route) != nil {
					t.Fatal("fixture")
				}
				route.FixSHA = ""
				admissionSQL(t, f, `UPDATE review_finding_routes SET body_json=? WHERE id=?`, encoded(route), r.ID)
			}
			_, err := f.e.ReadFindingRoute(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), r.ID)
			attemptCode(t, err, "storage_unavailable")
			_, err = routePending(t, f, 4)
			attemptCode(t, err, "storage_unavailable")
		})
	}
}

func TestFindingRouteAdjudicationV1MigrationAndRestartRetention(t *testing.T) {
	f, a, id := routeFixture(t, 1)
	adjudicationDecide(t, f, id, "legacy-decision", 0, 0, "accepted")
	before := adjudicationState(t, f, a)
	// The predecessor had neither route tables nor the later derived domain.
	// Remove only that optional domain before constructing its genuine shape.
	routePolicyPreDomain(t, f)
	admissionSQL(t, f, `DROP TABLE review_finding_route_revisions`)
	admissionSQL(t, f, `DROP TABLE review_finding_routes`)
	admissionSQL(t, f, `UPDATE metadata SET value='1' WHERE key='schema:review_adjudication'`)
	if err := EnsureAdjudicationSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if adjudicationState(t, f, a) != before {
		t.Fatal("migration rewrote source/history")
	}
	raw, err := f.e.DecideReviewFinding(t.Context(), f.scope(t, attemptProject, f.writer, false), "legacy-decision", id, FindingDecisionRequest{ExpectedRevision: adjudicationRevision(0), Finding: 0, Disposition: "accepted", Reason: "Explicit reason <script>literal</script>"})
	adjudicationReceipt(t, raw, err, id, "adjudicated", 1)
	if err := ensureRoutePolicySchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	r := routeCreate(t, f, id, "route-after-migration", routeRequest(1, 0, 4))
	initial := routeRead(t, f, r.ID)
	var file string
	if err = f.store.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&file); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.store = store
	if err = EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	f.e, err = New(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := routeRead(t, f, r.ID); !reflect.DeepEqual(got, initial) {
		t.Fatal("restart changed route", got, initial)
	}
}

func TestFindingRoutePendingCountAt100And101KeepsPageBound(t *testing.T) {
	f, a, id := routeFixture(t, 100)
	for i := range 100 {
		routeCreate(t, f, id, fmt.Sprintf("capacity-route-%d", i), routeRequest(int64(i), int64(i), 4))
	}
	if count, err := routePending(t, f, 4); err != nil || count != 100 {
		t.Fatal("100 valid pending routes not assessed exactly", count, err)
	}
	// A second genuine retained native capture supplies the 101st source finding;
	// no invented evidence row or malformed finding index is used for capacity.
	adjudicationComplete(t, f, a)
	receipt := f.enqueue(t, attemptProject, "second-route-source", "cli", 6, 2)
	next := f.start(t, attemptProject, receipt)
	anchor := attemptAnchor(attemptProject, 6)
	var err error
	next, err = f.e.Admit(t.Context(), attemptProject, next.ID, next.Revision, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	original, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
	if err != nil {
		t.Fatal(err)
	}
	capture := original.Capture
	capture.ExpectedAnchor = anchor
	capture.Review.Findings = capture.Review.Findings[:1]
	capture.Review.ReportedFindings = 1
	capture.CaptureSHA256 = strings.Repeat("2", 64)
	capture.CaptureID = "review_capture_" + capture.CaptureSHA256[:32]
	retained, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, next.ID, capture)
	if err != nil {
		t.Fatal(err)
	}
	routeCreate(t, f, retained.ID, "capacity-101", routeRequest(0, 0, 4))
	if count, err := routePending(t, f, 4); err != nil || count != 101 {
		t.Fatal("101 valid pending routes not assessed exactly", count, err)
	}
	page, err := f.e.ListFindingRoutes(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), 4, 100, "")
	if err != nil || len(page.Items) != 100 || !page.HasMore {
		t.Fatal("exact assessment changed the explicit page bound", len(page.Items), page.HasMore, err)
	}
}

func TestFindingRouteInvalidInputReplayAndReceivingDecisionRace(t *testing.T) {
	f, a, id := routeFixture(t, 1)
	scope := f.scope(t, attemptProject, f.writer, false)
	before := routeState(t, f, a)
	for name, change := range map[string]func(*FindingRouteRequest){
		"missingrevision": func(r *FindingRouteRequest) { r.ExpectedRevision = nil },
		"negativeguard":   func(r *FindingRouteRequest) { r.ExpectedRouteRevision = -1 },
		"samePR":          func(r *FindingRouteRequest) { r.TargetPullNumber = 2 },
		"unqueuedPR":      func(r *FindingRouteRequest) { r.TargetPullNumber = 999 },
		"missingfinding":  func(r *FindingRouteRequest) { r.Finding = 1 },
		"blankreason":     func(r *FindingRouteRequest) { r.Reason = "  " },
	} {
		t.Run(name, func(t *testing.T) {
			r := routeRequest(0, 0, 4)
			change(&r)
			_, err := f.e.CreateFindingRoute(t.Context(), scope, "invalid-"+name, id, r)
			attemptCode(t, err, "validation_failed")
			if routeState(t, f, a) != before {
				t.Fatal("bad request wrote state")
			}
		})
	}
	var wg sync.WaitGroup
	results := make(chan json.RawMessage, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			raw, err := f.e.CreateFindingRoute(t.Context(), scope, "same-route-key", id, routeRequest(0, 0, 4))
			results <- raw
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first json.RawMessage
	for raw := range results {
		if first == nil {
			first = raw
		} else if !bytes.Equal(first, raw) {
			t.Fatal("same key generated separate route")
		}
	}
	var route findingRouteReceipt
	if json.Unmarshal(first, &route) != nil {
		t.Fatal(string(first))
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_finding_routes`) != 1 || adjudicationRead(t, f, id).Revision != 1 {
		t.Fatal("same-key race had two effects")
	}
	changed := routeRequest(0, 0, 6)
	_, err := f.e.CreateFindingRoute(t.Context(), scope, "same-route-key", id, changed)
	attemptCode(t, err, "idempotency_conflict")
	errs = make(chan error, 2)
	for _, disposition := range []string{"accepted", "rejected"} {
		wg.Go(func() {
			_, err := f.e.DecideFindingRoute(t.Context(), scope, "receive-race-"+disposition, route.ID, FindingRouteDecisionRequest{ExpectedRevision: adjudicationRevision(1), Disposition: disposition, Reason: "Competing decision"})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	success, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else {
			attemptCode(t, err, "review_conflict")
			conflicts++
		}
	}
	if success != 1 || conflicts != 1 || routeRead(t, f, route.ID).Route.Revision != 2 {
		t.Fatal("receiving race lost guarded revision", success, conflicts)
	}
}

// Model a valid retained route created while the local wall clock was ahead.
// Keep every indexed origin, current revision and source-link timestamp exact.
func routeRetainedFuture(t *testing.T, f *attemptFixture, id string) {
	t.Helper()
	route := routeRead(t, f, id).Route
	future := "2099-01-01T00:00:00.000000000Z"
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(t.Context(), `SELECT body_json FROM review_finding_route_revisions WHERE route_id=? ORDER BY revision`, id)
	if err != nil {
		t.Fatal(err)
	}
	var history []ReviewRoute
	for rows.Next() {
		var body string
		if err = rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		var r ReviewRoute
		if err = json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatal(err)
		}
		r.CreatedAt = future
		r.UpdatedAt = future
		if r.DecidedAt != "" {
			r.DecidedAt = future
		}
		if r.FixedAt != "" {
			r.FixedAt = future
		}
		history = append(history, r)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, r := range history {
		if _, err = tx.ExecContext(t.Context(), `UPDATE review_finding_route_revisions SET body_json=?,created_at=? WHERE route_id=? AND revision=?`, encoded(r), r.UpdatedAt, id, r.Revision); err != nil {
			t.Fatal(err)
		}
		if r.Revision == route.Revision {
			if _, err = tx.ExecContext(t.Context(), `UPDATE review_finding_routes SET body_json=?,created_at=? WHERE id=?`, encoded(r), r.CreatedAt, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	var body string
	if err = tx.QueryRowContext(t.Context(), `SELECT body_json FROM review_adjudication_state WHERE evidence_id=?`, route.SourceEvidenceID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var source ReviewAdjudication
	if err = json.Unmarshal([]byte(body), &source); err != nil {
		t.Fatal(err)
	}
	source.Findings[route.Finding].DecidedAt = future
	if _, err = tx.ExecContext(t.Context(), `UPDATE review_adjudication_state SET body_json=? WHERE evidence_id=?`, encoded(source), route.SourceEvidenceID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(t.Context(), `UPDATE review_adjudication_changes SET created_at=? WHERE evidence_id=?`, future, route.SourceEvidenceID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	routeRead(t, f, id)
}

func TestFindingRouteClockRollbackDecisionAndFixKeepExactRevisions(t *testing.T) {
	f, a, evidence := routeFixture(t, 1)
	created := routeCreate(t, f, evidence, "route-clock", routeRequest(0, 0, 4))
	routeRetainedFuture(t, f, created.ID)
	source := adjudicationRead(t, f, evidence)
	routeDecision(t, f, created.ID, "decision-clock", 1, "accepted")
	decided := routeRead(t, f, created.ID).Route
	if decided.DecidedAt != decided.UpdatedAt || decided.UpdatedAt != decided.CreatedAt || decided.Revision != 2 {
		t.Fatal("decision lost effective journal time or sampled twice", decided)
	}
	before := routeState(t, f, a)
	_, err := f.e.DecideFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), "stale-new-key", created.ID, FindingRouteDecisionRequest{ExpectedRevision: adjudicationRevision(1), Disposition: "rejected", Reason: "Stale guard"})
	attemptCode(t, err, "review_conflict")
	if before != routeState(t, f, a) {
		t.Fatal("stale guard wrote")
	}
	routeRetainedFuture(t, f, created.ID)
	before = routeState(t, f, a)
	refused := fmt.Errorf("synthetic projection refusal")
	previousProjection := f.e.publishAdjudication
	f.e.publishAdjudication = func(context.Context, *sql.Tx, core.Scope, ReviewAdjudication) error { return refused }
	_, err = f.e.RecordFindingRouteFix(t.Context(), f.scope(t, attemptProject, f.writer, false), "rollback-clock-fix", created.ID, FindingRouteFixRequest{ExpectedRevision: adjudicationRevision(2), ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("f", 40), ProofNote: "Recorded focused proof"})
	f.e.publishAdjudication = previousProjection
	if err != refused || before != routeState(t, f, a) {
		t.Fatal("effective-time route mutation failed atomic rollback", err)
	}
	routeFix(t, f, created.ID, "fix-clock", 2, "", strings.Repeat("f", 40))
	fixed := routeRead(t, f, created.ID).Route
	if fixed.FixedAt != fixed.UpdatedAt || fixed.FixedAt != fixed.DecidedAt || fixed.Revision != 3 {
		t.Fatal("fix lost effective journal time or sampled twice", fixed)
	}
	if !reflect.DeepEqual(source, adjudicationRead(t, f, evidence)) {
		t.Fatal("receiver rewrote source")
	}
	before = routeState(t, f, a)
	raw, err := f.e.DecideFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), "decision-clock", created.ID, FindingRouteDecisionRequest{ExpectedRevision: adjudicationRevision(1), Disposition: "accepted", Reason: "Explicit receiving reason"})
	adjudicationReceipt(t, raw, err, created.ID, "adjudicated", 2)
	if before != routeState(t, f, a) {
		t.Fatal("exact replay rewrote later proof/history")
	}
	for revision := int64(1); revision <= 3; revision++ {
		var body string
		if err = f.store.DB().QueryRow(`SELECT body_json FROM review_finding_route_revisions WHERE route_id=? AND revision=?`, created.ID, revision).Scan(&body); err != nil {
			t.Fatal(err)
		}
		r, err := decodeReviewRoute(body)
		if err != nil || r.Revision != revision {
			t.Fatal("revision history lost after rollback", err)
		}
	}
}

func TestFindingRouteClockRollbackRerouteAndSourceUnroute(t *testing.T) {
	for _, unroute := range []bool{false, true} {
		t.Run(fmt.Sprint(unroute), func(t *testing.T) {
			f, a, evidence := routeFixture(t, 1)
			created := routeCreate(t, f, evidence, "origin", routeRequest(0, 0, 4))
			routeRetainedFuture(t, f, created.ID)
			if unroute {
				raw, err := f.e.DecideReviewFinding(t.Context(), f.scope(t, attemptProject, f.writer, false), "source-unroute", evidence, FindingDecisionRequest{ExpectedRevision: adjudicationRevision(1), ExpectedRouteRevision: 1, Finding: 0, Disposition: "rejected", Reason: "Explicit source correction"})
				adjudicationReceipt(t, raw, err, evidence, "adjudicated", 2)
				old := routeRead(t, f, created.ID).Route
				source := adjudicationRead(t, f, evidence).Findings[0]
				if old.State != "superseded" || old.UpdatedAt != old.CreatedAt || source.DecidedAt != old.UpdatedAt || source.RouteID != "" {
					t.Fatal("unroute stamp/link changed", old, source)
				}
			} else {
				request := routeRequest(1, 0, 6)
				request.ExpectedRouteRevision = 1
				next := routeCreate(t, f, evidence, "reroute", request)
				old := routeRead(t, f, created.ID).Route
				newRoute := routeRead(t, f, next.ID).Route
				source := adjudicationRead(t, f, evidence).Findings[0]
				if old.State != "superseded" || old.UpdatedAt != old.CreatedAt || old.UpdatedAt != newRoute.CreatedAt || source.DecidedAt != newRoute.CreatedAt {
					t.Fatal("reroute stamp/origin changed", old, newRoute, source)
				}
				before := routeState(t, f, a)
				replay := routeCreate(t, f, evidence, "reroute", request)
				if replay != next || before != routeState(t, f, a) {
					t.Fatal("reroute replay changed history")
				}
			}
		})
	}
}
