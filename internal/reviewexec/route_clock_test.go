package reviewexec

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFindingRouteClockRollbackReceiverHistoryAndExactReplay(t *testing.T) {
	f, a, captured, _ := routeProjectionFixture(t, true)
	evidence := captured.ID
	scope := f.scope(t, attemptProject, f.writer, false)
	clock := time.Date(2030, 1, 1, 12, 0, 0, 300000000, time.UTC)
	samples := 0
	f.e.routeNow = func() time.Time { samples++; return clock }
	created := routeCreate(t, f, evidence, "clock-create", routeRequest(0, 0, 4))
	initial := routeRead(t, f, created.ID).Route
	original := adjudicationRead(t, f, evidence)
	projectedOriginal := routeProjectionRead(t, f, evidence)
	oldDigest := projectionDigest(t, f, projectedOriginal.SnapshotID)
	clock = clock.Add(-250 * time.Millisecond)
	decision := FindingRouteDecisionRequest{ExpectedRevision: adjudicationRevision(1), Disposition: "accepted", Reason: "Receiving acceptance after clock step"}
	raw, err := f.e.DecideFindingRoute(t.Context(), scope, "clock-decision", created.ID, decision)
	adjudicationReceipt(t, raw, err, created.ID, "adjudicated", 2)
	decided := routeRead(t, f, created.ID).Route
	if decided.UpdatedAt != initial.UpdatedAt || decided.DecidedAt != decided.UpdatedAt {
		t.Fatal("decision did not use one effective recording stamp", decided)
	}
	after := routeProjectionDurableState(t, f, a)
	beforeSamples := samples
	replay, err := f.e.DecideFindingRoute(t.Context(), scope, "clock-decision", created.ID, decision)
	if err != nil || !bytes.Equal(raw, replay) || routeProjectionDurableState(t, f, a) != after || samples != beforeSamples {
		t.Fatal("exact decision retry resampled time or changed retained history", err)
	}
	clock = clock.Add(-time.Second)
	empty := ""
	fix := FindingRouteFixRequest{ExpectedRevision: adjudicationRevision(2), ExpectedSHA: &empty, FixSHA: strings.Repeat("f", 40), ProofNote: "Focused receiver proof after clock step"}
	raw, err = f.e.RecordFindingRouteFix(t.Context(), scope, "clock-fix", created.ID, fix)
	adjudicationReceipt(t, raw, err, created.ID, "fix_recorded", 3)
	fixed := routeRead(t, f, created.ID).Route
	if fixed.FixedAt != initial.UpdatedAt || fixed.UpdatedAt != fixed.FixedAt || fixed.DecidedAt != decided.DecidedAt {
		t.Fatal("fix time lost retained chronology", fixed)
	}
	after = routeProjectionDurableState(t, f, a)
	beforeSamples = samples
	replay, err = f.e.RecordFindingRouteFix(t.Context(), scope, "clock-fix", created.ID, fix)
	if err != nil || !bytes.Equal(raw, replay) || routeProjectionDurableState(t, f, a) != after || samples != beforeSamples {
		t.Fatal("exact fix retry changed history or sampled clock", err)
	}
	if !reflect.DeepEqual(original, adjudicationRead(t, f, evidence)) {
		t.Fatal("receiving clock correction rewrote source adjudication")
	}
	projected := routeProjectionRead(t, f, evidence)
	if projected.Findings[0].Route == nil || projected.Findings[0].Route.Revision != 3 || projected.Findings[0].Route.FixedAt != fixed.FixedAt || projected.Findings[0].Route.DecidedAt != decided.DecidedAt || projected.Findings[0].Detail != projectedOriginal.Findings[0].Detail {
		t.Fatal("native source projection lost effective receiver facts", projected.Findings[0])
	}
	if projectionDigest(t, f, projectedOriginal.SnapshotID) != oldDigest {
		t.Fatal("receiver clock correction rewrote historical source snapshot")
	}
	var originalBody string
	if err = f.store.DB().QueryRow(`SELECT body_json FROM review_finding_route_revisions WHERE route_id=? AND revision=1`, created.ID).Scan(&originalBody); err != nil {
		t.Fatal(err)
	}
	if originalBody != encoded(initial) {
		t.Fatal("old route revision bytes changed")
	}
}

func TestFindingRouteClockSingleSampleWithinReceiverMutation(t *testing.T) {
	f, _, evidence := routeFixture(t, 1)
	base := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	f.e.routeNow = func() time.Time { return base }
	r := routeCreate(t, f, evidence, "within-create", routeRequest(0, 0, 4))
	samples := 0
	f.e.routeNow = func() time.Time {
		samples++
		return base.Add(time.Second - time.Duration(samples-1)*250*time.Millisecond)
	}
	routeDecision(t, f, r.ID, "within-decision", 1, "accepted")
	decided := routeRead(t, f, r.ID).Route
	if samples != 1 || decided.DecidedAt != decided.UpdatedAt {
		t.Fatalf("decision sampled local clock %d times or disagreed: %+v", samples, decided)
	}
	samples = 0
	routeFix(t, f, r.ID, "within-fix", 2, "", strings.Repeat("a", 40))
	fixed := routeRead(t, f, r.ID).Route
	if samples != 1 || fixed.FixedAt != fixed.UpdatedAt {
		t.Fatalf("fix sampled local clock %d times or disagreed: %+v", samples, fixed)
	}
}

func TestFindingRouteClockRollbackRerouteAndSourceUnrouteKeepExactLinks(t *testing.T) {
	f, a, evidence := routeFixture(t, 1)
	scope := f.scope(t, attemptProject, f.writer, false)
	clock := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	samples := 0
	f.e.routeNow = func() time.Time { samples++; return clock }
	first := routeCreate(t, f, evidence, "reroute-clock-create", routeRequest(0, 0, 4))
	routeDecision(t, f, first.ID, "reroute-clock-receive", 1, "accepted")
	old := routeRead(t, f, first.ID).Route
	clock = clock.Add(-time.Second)
	request := routeRequest(1, 0, 6)
	request.ExpectedRouteRevision = 2
	beforeSamples := samples
	raw, err := f.e.CreateFindingRoute(t.Context(), scope, "reroute-clock", evidence, request)
	if samples != beforeSamples+1 {
		t.Fatal("reroute sampled local clock more than once", samples-beforeSamples)
	}
	var next findingRouteReceipt
	if err != nil || json.Unmarshal(raw, &next) != nil {
		t.Fatal("reroute after clock step", err)
	}
	previous := routeRead(t, f, first.ID).Route
	current := routeRead(t, f, next.ID).Route
	source := adjudicationRead(t, f, evidence)
	if previous.State != "superseded" || previous.UpdatedAt != old.UpdatedAt || current.CreatedAt != previous.UpdatedAt || current.CreatedAt != current.UpdatedAt || source.Findings[0].DecidedAt != current.CreatedAt || source.Findings[0].RouteID != current.ID {
		t.Fatal("reroute lost effective-time source link", previous, current, source)
	}
	after := routeState(t, f, a)
	beforeSamples = samples
	replay, err := f.e.CreateFindingRoute(t.Context(), scope, "reroute-clock", evidence, request)
	if err != nil || !bytes.Equal(raw, replay) || routeState(t, f, a) != after || samples != beforeSamples {
		t.Fatal("reroute replay changed history or clock", err)
	}
	clock = clock.Add(-time.Second)
	decision := FindingDecisionRequest{ExpectedRevision: adjudicationRevision(2), Finding: 0, Disposition: "rejected", Reason: "Source correction after clock step", ExpectedRouteRevision: 1}
	beforeSamples = samples
	raw, err = f.e.DecideReviewFinding(t.Context(), scope, "unroute-clock", evidence, decision)
	if samples != beforeSamples+1 {
		t.Fatal("source unroute sampled route clock more than once", samples-beforeSamples)
	}
	adjudicationReceipt(t, raw, err, evidence, "adjudicated", 3)
	superseded := routeRead(t, f, next.ID).Route
	source = adjudicationRead(t, f, evidence)
	if superseded.State != "superseded" || superseded.UpdatedAt != current.UpdatedAt || source.Findings[0].RouteID != "" || source.Findings[0].DecidedAt != superseded.UpdatedAt {
		t.Fatal("source unroute lost effective-time link", superseded, source)
	}
	after = routeState(t, f, a)
	beforeSamples = samples
	replay, err = f.e.DecideReviewFinding(t.Context(), scope, "unroute-clock", evidence, decision)
	if err != nil || !bytes.Equal(raw, replay) || routeState(t, f, a) != after || samples != beforeSamples {
		t.Fatal("unroute replay changed history or clock", err)
	}
}
