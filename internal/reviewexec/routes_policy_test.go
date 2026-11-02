package reviewexec

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// The receiving capture is genuine journal evidence, independent of its incoming
// obligations. Source observation uses the owned worker before read-only checks.
func routePolicyReceiver(t *testing.T, f *attemptFixture, sourceID string) (Attempt, string, *refreshTestReader) {
	t.Helper()
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "route-policy-receiver", "cli", 4, 2))
	anchor := attemptAnchor(attemptProject, 4)
	anchor.ParentSHA = anchor.BaseSHA
	var err error
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	original, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), sourceID)
	if err != nil {
		t.Fatal(err)
	}
	capture := evidenceClone(original.Capture)
	capture.ExpectedAnchor = anchor
	capture.CaptureSHA256 = strings.Repeat("7", 64)
	capture.CaptureID = "review_capture_" + capture.CaptureSHA256[:32]
	capture.Review.Findings = capture.Review.Findings[:0]
	capture.Review.ReportedFindings = 0
	receipt, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, capture)
	if err != nil {
		t.Fatal(err)
	}
	adjudicationComplete(t, f, a)
	raw, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "route-policy-finalize-receiver", receipt.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(0)})
	adjudicationReceipt(t, raw, err, receipt.ID, "finalized", 1)
	assessmentPolicy(t, f, 1, []string{"cli"}, []string{})
	reader := &refreshTestReader{}
	queued := refreshEnqueue(t, f, "route-policy-observe", SourceRefreshRequest{ExpectedConfigurationRevision: 2, PullNumber: 4, HeadSHA: anchor.CandidateSHA})
	cancel, done := refreshStart(t, f, reader)
	refreshWait(t, f, queued.ID, "completed")
	refreshStop(t, cancel, done)
	return a, receipt.ID, reader
}

func routePolicyBoth(t *testing.T, f *attemptFixture, a Attempt, id string, reader *refreshTestReader, state, reason string, pending int64, capacity bool) {
	t.Helper()
	before, calls := routeState(t, f, a), reader.calls.Load()
	captured, err := f.e.ReadPolicyAssessment(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
	if err != nil {
		t.Fatal("captured assessment", err)
	}
	observed, err := f.e.ReadSourcePolicyAssessment(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), 4)
	if err != nil {
		t.Fatal("source assessment", err)
	}
	wantChannels := []PolicyAssessmentChannel{{Channel: "cli", Covered: true}}
	if captured.State != state || observed.State != state || captured.Reason != reason || observed.Reason != reason || captured.IncomingRoutesPending != pending || observed.IncomingRoutesPending != pending || captured.IncomingRoutesCapacity != capacity || observed.IncomingRoutesCapacity != capacity || !reflect.DeepEqual(captured.Channels, wantChannels) || !reflect.DeepEqual(observed.Channels, wantChannels) {
		t.Fatalf("incoming obligations must be separate from channel credit: captured=%+v observed=%+v", captured, observed)
	}
	if calls != reader.calls.Load() || before != routeState(t, f, a) {
		t.Fatal("policy read called provider or changed durable state")
	}
}

func TestRoutePolicyReceivingCoverageDoesNotClearIncomingObligation(t *testing.T) {
	f, source, sourceID := routeFixture(t, 1)
	adjudicationComplete(t, f, source)
	a, id, reader := routePolicyReceiver(t, f, sourceID)
	routePolicyBoth(t, f, a, id, reader, "sufficient", "", 0, false)
	created := routeCreate(t, f, sourceID, "policy-incoming", routeRequest(0, 0, 4))
	raw, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "policy-source-finalize", sourceID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
	adjudicationReceipt(t, raw, err, sourceID, "finalized", 2)
	original := adjudicationRead(t, f, sourceID)
	capture, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), sourceID)
	if err != nil {
		t.Fatal(err)
	}
	routePolicyBoth(t, f, a, id, reader, "held", "routed_findings_pending", 1, false)
	routeDecision(t, f, created.ID, "policy-accept", 1, "accepted")
	routePolicyBoth(t, f, a, id, reader, "held", "routed_findings_pending", 1, false)
	routeDecision(t, f, created.ID, "policy-reject", 2, "rejected")
	routePolicyBoth(t, f, a, id, reader, "sufficient", "", 0, false)
	routeDecision(t, f, created.ID, "policy-correct-accept", 3, "accepted")
	routePolicyBoth(t, f, a, id, reader, "held", "routed_findings_pending", 1, false)
	routeFix(t, f, created.ID, "policy-fix", 4, "", strings.Repeat("d", 40))
	routePolicyBoth(t, f, a, id, reader, "sufficient", "", 0, false)
	after, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), sourceID)
	if err != nil || !reflect.DeepEqual(capture, after) || !reflect.DeepEqual(original, adjudicationRead(t, f, sourceID)) || original.FrozenCounts == nil || *original.FrozenCounts != (FindingCounts{Found: 1, Routed: 1}) {
		t.Fatal("receiver changed source attribution/revision/frozen counts/capture", err)
	}
	// Configuration staleness is a separate hold, not misreported as incoming work.
	f.configure(t, attemptProject, "other-account", 2, false)
	stale, err := f.e.ReadSourcePolicyAssessment(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), 4)
	if err != nil || stale.State != "held" || stale.Reason != "source_configuration_changed" || len(stale.Channels) != 0 || stale.IncomingRoutesPending != 0 || stale.IncomingRoutesCapacity {
		t.Fatal("stale source confused with routing", stale, err)
	}
}

func TestRoutePolicyOver100PendingRemainsExactHold(t *testing.T) {
	f, source, sourceID := routeFixture(t, 100)
	adjudicationComplete(t, f, source)
	a, id, reader := routePolicyReceiver(t, f, sourceID)
	for i := range 100 {
		routeCreate(t, f, sourceID, fmt.Sprintf("policy-cap-%d", i), routeRequest(int64(i), int64(i), 4))
	}
	routePolicyBoth(t, f, a, id, reader, "held", "routed_findings_pending", 100, false)
	next := f.start(t, attemptProject, f.enqueue(t, attemptProject, "policy-extra-source", "cli", 6, 2))
	anchor := attemptAnchor(attemptProject, 6)
	var err error
	next, err = f.e.Admit(t.Context(), attemptProject, next.ID, next.Revision, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	original, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), sourceID)
	if err != nil {
		t.Fatal(err)
	}
	capture := evidenceClone(original.Capture)
	capture.ExpectedAnchor = anchor
	capture.CaptureSHA256 = strings.Repeat("8", 64)
	capture.CaptureID = "review_capture_" + capture.CaptureSHA256[:32]
	capture.Review.Findings = capture.Review.Findings[:1]
	capture.Review.ReportedFindings = 1
	receipt, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, next.ID, capture)
	if err != nil {
		t.Fatal(err)
	}
	routeCreate(t, f, receipt.ID, "policy-cap-101", routeRequest(0, 0, 4))
	routePolicyBoth(t, f, a, id, reader, "held", "routed_findings_pending", 101, false)
}

func TestRoutePolicyCorruptResolvedIncomingRouteFailsClosed(t *testing.T) {
	f, source, sourceID := routeFixture(t, 1)
	adjudicationComplete(t, f, source)
	a, id, reader := routePolicyReceiver(t, f, sourceID)
	created := routeCreate(t, f, sourceID, "policy-corrupt-route", routeRequest(0, 0, 4))
	routeDecision(t, f, created.ID, "policy-valid-rejection", 1, "rejected")
	routePolicyBoth(t, f, a, id, reader, "sufficient", "", 0, false)
	// Keep current history/body equal, but remove mandatory receiving justification.
	route := routeRead(t, f, created.ID).Route
	route.Reason = ""
	body, _ := json.Marshal(route)
	admissionSQL(t, f, `UPDATE review_finding_routes SET body_json=? WHERE id=?`, string(body), route.ID)
	admissionSQL(t, f, `UPDATE review_finding_route_revisions SET body_json=? WHERE route_id=? AND revision=?`, string(body), route.ID, route.Revision)
	before, calls := routeState(t, f, a), reader.calls.Load()
	captured, err := f.e.ReadPolicyAssessment(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
	if err == nil || captured.State == "sufficient" {
		t.Fatal("corrupt route credited", captured, err)
	}
	observed, err := f.e.ReadSourcePolicyAssessment(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), 4)
	if err == nil || observed.State == "sufficient" {
		t.Fatal("corrupt route credited", observed, err)
	}
	if before != routeState(t, f, a) || calls != reader.calls.Load() {
		t.Fatal("failed read changed state or called provider")
	}
}
