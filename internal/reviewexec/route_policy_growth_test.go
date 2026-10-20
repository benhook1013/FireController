package reviewexec

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestRoutePolicyGrowthExactPendingResolvedAndReceiverCorrections(t *testing.T) {
	// Both captures stay below the existing100-finding bound. Completing the
	// first source releases its execution slot; no authority rows are fabricated.
	f, firstAttempt, firstID := routeFixture(t, 52)
	adjudicationComplete(t, f, firstAttempt)
	// Establish genuine receiving coverage before incoming obligations arrive;
	// new direct requests are held while receiving routes remain pending.
	receiver, receiverID, reader := routePolicyReceiver(t, f, firstID)
	routes := make([]findingRouteReceipt, 0, 103)
	for i := range 52 {
		routes = append(routes, routeCreate(t, f, firstID, fmt.Sprintf("growth-first-%d", i), routeRequest(int64(i), int64(i), 4)))
	}
	raw, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "growth-finalize-first", firstID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(52)})
	adjudicationReceipt(t, raw, err, firstID, "finalized", 53)

	secondAttempt := f.start(t, attemptProject, f.enqueue(t, attemptProject, "growth-second-source", "cli", 6, 2))
	anchor := attemptAnchor(attemptProject, 6)
	secondAttempt, err = f.e.Admit(t.Context(), attemptProject, secondAttempt.ID, secondAttempt.Revision, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	firstCapture, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), firstID)
	if err != nil {
		t.Fatal(err)
	}
	capture := evidenceClone(firstCapture.Capture)
	capture.ExpectedAnchor = anchor
	capture.CaptureSHA256 = strings.Repeat("9", 64)
	capture.CaptureID = "review_capture_" + capture.CaptureSHA256[:32]
	capture.Review.Findings = capture.Review.Findings[:51]
	capture.Review.ReportedFindings = 51
	second, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, secondAttempt.ID, capture)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 51 {
		routes = append(routes, routeCreate(t, f, second.ID, fmt.Sprintf("growth-second-%d", i), routeRequest(int64(i), int64(i), 4)))
	}
	secondCapture, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstSource, secondSource := adjudicationRead(t, f, firstID), adjudicationRead(t, f, second.ID)
	if firstSource.FrozenCounts == nil || *firstSource.FrozenCounts != (FindingCounts{Found: 52, Routed: 52}) || len(secondSource.Findings) != 51 {
		t.Fatal("genuine source counts missing", firstSource, secondSource)
	}
	for _, finding := range secondSource.Findings {
		if finding.Disposition != "routed" {
			t.Fatal("second source did not retain every routed attribution")
		}
	}
	routePolicyBoth(t, f, receiver, receiverID, reader, "held", "routed_findings_pending", 103, false)
	for _, target := range []int64{2, 6} {
		if count, err := routePending(t, f, target); err != nil || count != 0 {
			t.Fatal("incoming routes crossed receiving scopes", target, count, err)
		}
	}

	// Ordinary incoming inspection remains a bounded explicit continuation.
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	before := routeState(t, f, receiver)
	page, err := f.e.ListFindingRoutes(t.Context(), scope, 4, 100, "")
	if err != nil || len(page.Items) != 100 || !page.HasMore || page.Cursor == "" {
		t.Fatal("first bounded route page", len(page.Items), page.HasMore, err)
	}
	tail, err := f.e.ListFindingRoutes(t.Context(), scope, 4, 100, page.Cursor)
	if err != nil || len(tail.Items) != 3 || tail.HasMore {
		t.Fatal("route continuation lost retained routes", len(tail.Items), tail.HasMore, err)
	}
	seen := map[string]bool{}
	for _, route := range append(page.Items, tail.Items...) {
		if seen[route.ID] || route.TargetPullNumber != 4 {
			t.Fatal("duplicate or foreign route in continuation")
		}
		seen[route.ID] = true
	}
	_, err = f.e.ListFindingRoutes(t.Context(), scope, 4, 101, "")
	attemptCode(t, err, "validation_failed")
	if before != routeState(t, f, receiver) {
		t.Fatal("bounded route reads changed authority or policy facts")
	}

	// Keep the oldest route pending behind102 newer active, resolved routes.
	// A newest100 window would incorrectly report no outstanding work here.
	for i := 1; i < len(routes); i++ {
		routeDecision(t, f, routes[i].ID, fmt.Sprintf("growth-resolve-%d", i), 1, "rejected")
	}
	routePolicyBoth(t, f, receiver, receiverID, reader, "held", "routed_findings_pending", 1, false)
	routeDecision(t, f, routes[0].ID, "growth-oldest-accept", 1, "accepted")
	routePolicyBoth(t, f, receiver, receiverID, reader, "held", "routed_findings_pending", 1, false)
	routeDecision(t, f, routes[0].ID, "growth-oldest-reject", 2, "rejected")
	routePolicyBoth(t, f, receiver, receiverID, reader, "sufficient", "", 0, false)
	if !reflect.DeepEqual(firstSource, adjudicationRead(t, f, firstID)) || !reflect.DeepEqual(secondSource, adjudicationRead(t, f, second.ID)) {
		t.Fatal("receiver resolution rewrote source attribution or counts")
	}

	adjudicationComplete(t, f, secondAttempt)
	raw, err = f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "growth-finalize-second", second.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(51)})
	adjudicationReceipt(t, raw, err, second.ID, "finalized", 52)
	secondSource = adjudicationRead(t, f, second.ID)
	if secondSource.FrozenCounts == nil || *secondSource.FrozenCounts != (FindingCounts{Found: 51, Routed: 51}) {
		t.Fatal("second source did not freeze original counts", secondSource)
	}
	routeDecision(t, f, routes[0].ID, "growth-reopen-oldest", 3, "accepted")
	routePolicyBoth(t, f, receiver, receiverID, reader, "held", "routed_findings_pending", 1, false)
	routeFix(t, f, routes[0].ID, "growth-fix-oldest", 4, "", strings.Repeat("d", 40))
	routePolicyBoth(t, f, receiver, receiverID, reader, "sufficient", "", 0, false)
	before = routeState(t, f, receiver)
	raw, err = f.e.DecideFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), "growth-reopen-oldest", routes[0].ID, FindingRouteDecisionRequest{ExpectedRevision: adjudicationRevision(3), Disposition: "accepted", Reason: "Explicit receiving reason"})
	adjudicationReceipt(t, raw, err, routes[0].ID, "adjudicated", 4)
	if before != routeState(t, f, receiver) {
		t.Fatal("old exact retry changed later receiving fix or policy facts")
	}
	for _, source := range []struct {
		id      string
		state   ReviewAdjudication
		capture ReviewEvidence
	}{{firstID, firstSource, firstCapture}, {second.ID, secondSource, secondCapture}} {
		got, err := f.e.ReadReviewEvidence(t.Context(), scope, source.id)
		if err != nil || !reflect.DeepEqual(got, source.capture) || !reflect.DeepEqual(adjudicationRead(t, f, source.id), source.state) {
			t.Fatal("receiver correction changed source capture/frozen attribution", source.id, err)
		}
	}
	if err := ValidateRoutePolicyFacts(t.Context(), f.store.DB()); err != nil {
		t.Fatal("supported route lifecycle left invalid derived policy facts", err)
	}
}
