package reviews

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func nativeRouteFixture(index int) *FindingRoute {
	return &FindingRoute{ID: fmt.Sprintf("review_route_%032x", index+1), TargetPullNumber: 2, Revision: 1, State: "active", Disposition: "unadjudicated"}
}

func TestNativeRoutesPreserveFrozenCountsSourceAndSnapshotHistory(t *testing.T) {
	f, input, prior := nativeAdjudicationFixture(t)
	old := nativeSnapshotBytes(t, f, prior.ID)
	decideNativeFinding(&input.Findings[0], "routed")
	input.Findings[0].Route = nativeRouteFixture(0)
	decideNativeFinding(&input.Findings[1], "rejected")
	first, changed, err := applyNativeAdjudication(f, input)
	if err != nil || !changed {
		t.Fatal(first, changed, err)
	}
	runID := runOpaqueID(f.project, nativeSource, input.SourceRunID)
	var firstDetail RunDetailResponse
	firstBody := nativeRead(t, f, "/runs/"+runID+"?snapshot="+first.ID, &firstDetail)
	if firstDetail.Run.Routed != 1 || firstDetail.Run.Accepted != 0 || firstDetail.Findings[0].Route == nil || firstDetail.Findings[0].FixSHA != "" {
		t.Fatal("route lost or promoted to source fix", firstDetail)
	}
	input.Finalized = true
	input.FrozenCounts = &NativeCounts{Found: 2, Routed: 1}
	if _, _, err = applyNativeAdjudication(f, input); err != nil {
		t.Fatal("source finalization required receiver completion", err)
	}
	for _, decision := range []string{"rejected", "accepted"} {
		route := *input.Findings[0].Route
		route.Revision++
		route.Disposition = decision
		route.Reason = "Independent receiving decision <literal>"
		route.Actor = "receiving-actor"
		route.DecidedAt = "2026-01-01T00:05:00Z"
		input.Findings[0].Route = &route
		if _, changed, err = applyNativeAdjudication(f, input); err != nil || !changed {
			t.Fatal("receiving change did not project", changed, err)
		}
	}
	route := *input.Findings[0].Route
	route.Revision++
	route.FixSHA = strings.Repeat("d", 40)
	route.ProofNote = "Receiving proof <b>literal</b>"
	route.FixedAt = "2026-01-01T00:06:00Z"
	input.Findings[0].Route = &route
	current, changed, err := applyNativeAdjudication(f, input)
	if err != nil || !changed {
		t.Fatal("receiver fix", current, changed, err)
	}
	var detail RunDetailResponse
	body := nativeRead(t, f, "/runs/"+runID, &detail)
	if detail.Run.Found != 2 || detail.Run.Accepted != 0 || detail.Run.Routed != 1 || detail.Run.Fixed != 0 || !detail.Run.Finalized || detail.Findings[0].Actor != input.Findings[0].Actor || detail.Findings[0].DecisionReason != input.Findings[0].DecisionReason || detail.Findings[0].Route.Actor != "receiving-actor" || detail.Findings[0].Route.FixSHA != route.FixSHA {
		t.Fatal("receiver rewrote frozen source identity/counts", detail)
	}
	if strings.Contains(string(body), "<b>literal</b>") || !strings.Contains(string(body), `\u003c`) {
		t.Fatal("literal proof was not safely JSON escaped")
	}
	counts := nativeProjectionCounts(t, f)
	replay, changed, err := applyNativeAdjudication(f, input)
	if err != nil || changed || replay != current || !reflect.DeepEqual(counts, nativeProjectionCounts(t, f)) {
		t.Fatal("equal route DTO allocated new snapshot", replay, changed, err)
	}
	var historical RunDetailResponse
	if string(nativeRead(t, f, "/runs/"+runID+"?snapshot="+first.ID, &historical)) != string(firstBody) || nativeSnapshotBytes(t, f, prior.ID) != old {
		t.Fatal("receiver update rewrote retained source snapshot")
	}
	// An unrelated native publication copies route_json with the retained finding.
	attempt := nativeAttempt("copy-route-snapshot", "active")
	mustNativePublish(t, f, NativePublication{Pull: nativePull(2), Attempt: &attempt})
	nativeRead(t, f, "/runs/"+runID, &detail)
	if !reflect.DeepEqual(detail.Findings[0].Route, &route) {
		t.Fatal("snapshot copy dropped route", detail.Findings[0])
	}
}

func TestNativeRoutesLegacyImportsAndMalformedNativeProof(t *testing.T) {
	f := newAPIFixture(t)
	legacy := nativeRun("legacy-routed")
	legacy.Accepted, legacy.Routed = 0, 1
	legacy.Findings[0].Disposition = "routed"
	legacy.Findings[0].FixSHA, legacy.Findings[0].ProofNote, legacy.Findings[0].ResolvedAt = "", "", ""
	pull := nativePull(1)
	pull.Runs = []RunInput{legacy}
	f.observe(t, reviewRequest(0, []PullInput{pull}), "import-routed-history")
	var detail RunDetailResponse
	nativeRead(t, f, "/runs/"+runOpaqueID(f.project, "test-import", legacy.SourceRunID), &detail)
	if detail.Findings[0].Route != nil || detail.Run.Routed != 1 {
		t.Fatal("legacy import fabricated native route", detail)
	}
	run := nativeUndecidedRun("invalid-native-route", 1)
	run.Findings[0].Disposition = "routed"
	run.Findings[0].DecisionReason = "Source routing"
	run.Findings[0].Actor = "source-actor"
	run.Findings[0].DecidedAt = "2026-01-01T00:02:00Z"
	counts := nativeProjectionCounts(t, f)
	for _, mode := range []string{"missing", "same-target", "invalid-id", "unadjudicated-proof", "nonrouted"} {
		row := run
		row.Findings = append([]Finding(nil), run.Findings...)
		row.Findings[0].Route = nativeRouteFixture(0)
		switch mode {
		case "missing":
			row.Findings[0].Route = nil
		case "same-target":
			row.Findings[0].Route.TargetPullNumber = 1
		case "invalid-id":
			row.Findings[0].Route.ID = "review_route_wrong"
		case "unadjudicated-proof":
			row.Findings[0].Route.ProofNote = "unproven"
		case "nonrouted":
			row.Findings[0].Disposition = "accepted"
		}
		if _, err := nativePublish(f, NativePublication{Pull: nativePull(1), Run: &row}); err == nil || !reflect.DeepEqual(counts, nativeProjectionCounts(t, f)) {
			t.Fatal("invalid route admitted or changed projection", mode, err)
		}
	}
	run.Findings[0].Route = nativeRouteFixture(0)
	current := mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
	id := runOpaqueID(f.project, nativeSource, run.SourceRunID)
	var original string
	if err := f.store.DB().QueryRow(`SELECT route_json FROM review_findings WHERE project_id=? AND snapshot_id=? AND run_id=?`, f.project, current.ID, id).Scan(&original); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{}`, `{"id":null}`, strings.Replace(original, `"revision":1`, `"revision":null`, 1), strings.Replace(original, `"revision":1`, `"revision":1,"extra":true`, 1)} {
		if _, err := f.store.DB().Exec(`UPDATE review_findings SET route_json=? WHERE project_id=? AND snapshot_id=? AND run_id=?`, body, f.project, current.ID, id); err != nil {
			t.Fatal(err)
		}
		w := f.call(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/runs/"+id, f.readToken, "", nil)
		if w.Code != 500 || !strings.Contains(w.Body.String(), "storage_unavailable") {
			t.Fatal("corrupt stored route presented as proof", w.Code, w.Body.String())
		}
	}
}

func TestNativeRoutesNestedProofParticipatesInDetailBudget(t *testing.T) {
	f := newAPIFixture(t)
	run := largeDetailRun()
	prior := mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
	before := nativeSnapshotBytes(t, f, prior.ID)
	counts := nativeProjectionCounts(t, f)
	input := nativeAdjudicationInput(run)
	for i := range input.Findings {
		finding := &input.Findings[i]
		decideNativeFinding(finding, "routed")
		finding.DecisionReason = strings.Repeat("<", 2048)
		route := nativeRouteFixture(i)
		route.Revision = 2
		route.Disposition = "accepted"
		route.Reason = strings.Repeat("<", 2048)
		route.Actor = "receiver"
		route.DecidedAt = "2026-01-01T00:05:00Z"
		route.FixSHA = strings.Repeat("f", 40)
		route.ProofNote = strings.Repeat("<", 2048)
		route.FixedAt = "2026-01-01T00:06:00Z"
		finding.Route = route
	}
	// Oversized proof is knowable before writing a new snapshot. Refuse it
	// before snapshot copies and per-finding updates can consume admission.
	if _, err := f.store.DB().Exec(`CREATE TRIGGER forbid_oversized_snapshot BEFORE INSERT ON review_snapshots BEGIN SELECT RAISE(ABORT,'unexpected snapshot write'); END`); err != nil {
		t.Fatal(err)
	}
	_, _, err := applyNativeAdjudication(f, input)
	nativeErrorCode(t, err, "workspace_capacity")
	if !reflect.DeepEqual(counts, nativeProjectionCounts(t, f)) || nativeSnapshotBytes(t, f, prior.ID) != before {
		t.Fatal("oversized nested proof partially published")
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER forbid_oversized_snapshot`); err != nil {
		t.Fatal(err)
	}
	for i := range input.Findings {
		input.Findings[i].DecisionReason = "Bounded source reason"
		input.Findings[i].Route.Reason = "Receiver rationale"
		input.Findings[i].Route.ProofNote = "Literal receiving proof"
	}
	if _, _, err = applyNativeAdjudication(f, input); err != nil {
		t.Fatal(err)
	}
	var detail RunDetailResponse
	nativeRead(t, f, "/runs/"+runOpaqueID(f.project, nativeSource, run.SourceRunID), &detail)
	if len(detail.Findings) != 100 || detail.Run.Routed != 100 || detail.Run.Fixed != 0 || detail.Findings[99].Route.ProofNote != "Literal receiving proof" {
		t.Fatal("nested routes truncated or counted as source fixes", detail.Run)
	}
}

func TestNativeRoutesKnownReceiverProofCannotBeSilentlyLost(t *testing.T) {
	f, input, _ := nativeAdjudicationFixture(t)
	decideNativeFinding(&input.Findings[0], "routed")
	decideNativeFinding(&input.Findings[1], "rejected")
	route := nativeRouteFixture(0)
	route.Revision = 3
	route.Disposition = "accepted"
	route.Reason = "Receiver accepted"
	route.Actor = "receiver"
	route.DecidedAt = "2026-01-01T00:05:00Z"
	route.FixSHA = strings.Repeat("d", 40)
	route.ProofNote = "Retained receiving proof"
	route.FixedAt = "2026-01-01T00:06:00Z"
	input.Findings[0].Route = route
	if _, _, err := applyNativeAdjudication(f, input); err != nil {
		t.Fatal(err)
	}
	counts := nativeProjectionCounts(t, f)
	for _, mode := range []string{"clear-proof", "change-route", "change-target", "reject-receiver", "replace-source"} {
		next := input
		next.Findings = append([]NativeFindingUpdate(nil), input.Findings...)
		copy := *route
		copy.Revision++
		next.Findings[0].Route = &copy
		switch mode {
		case "clear-proof":
			copy.FixSHA, copy.ProofNote, copy.FixedAt = "", "", ""
		case "change-route":
			copy.ID = fmt.Sprintf("review_route_%032x", 99)
		case "change-target":
			copy.TargetPullNumber = 3
		case "reject-receiver":
			copy.Disposition = "rejected"
			copy.FixSHA, copy.ProofNote, copy.FixedAt = "", "", ""
		case "replace-source":
			next.Findings[0].Disposition = "accepted"
			next.Findings[0].Route = nil
		}
		_, _, err := applyNativeAdjudication(f, next)
		nativeErrorCode(t, err, "observation_conflict")
		if !reflect.DeepEqual(counts, nativeProjectionCounts(t, f)) {
			t.Fatal("proof guard partially published", mode)
		}
	}
	correction := *route
	correction.Revision++
	correction.FixSHA = strings.Repeat("e", 40)
	correction.ProofNote = "Explicit corrected receiving proof"
	input.Findings[0].Route = &correction
	if _, changed, err := applyNativeAdjudication(f, input); err != nil || !changed {
		t.Fatal("valid receiving proof correction refused", changed, err)
	}
}

func TestImportedFindingRoutesMatchNativeReceiverProofContract(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*FindingRoute)
		valid  bool
	}{
		{"superseded_with_fix", func(r *FindingRoute) { r.State = "superseded" }, false},
		{"actor_with_spaces", func(r *FindingRoute) { r.Actor = "receiving actor" }, false},
		{"actor_non_native_prefix", func(r *FindingRoute) { r.Actor = "_receiving" }, false},
		{"active_fixed", func(*FindingRoute) {}, true},
		{"superseded_without_fix", func(r *FindingRoute) { r.State = "superseded"; r.FixSHA, r.ProofNote, r.FixedAt = "", "", "" }, true},
		{"maximum_native_actor", func(r *FindingRoute) { r.Actor = "a" + strings.Repeat("_-A1", 31) + "_-A" }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAPIFixture(t)
			run := importedRoutedRun()
			// A real administrator observation and public read establish the
			// valid blank-actor unadjudicated baseline before each attempted import.
			undecided := nativeRouteFixture(0)
			run.Findings[0].Route = undecided
			pull := nativePull(1)
			pull.Runs = []RunInput{run}
			first := f.observe(t, reviewRequest(0, []PullInput{pull}), "import-route-original")
			id := runOpaqueID(f.project, "test-import", run.SourceRunID)
			var original RunDetailResponse
			before := nativeRead(t, f, "/runs/"+id, &original)
			if original.Findings[0].Route.Actor != "" || original.Findings[0].Route.Disposition != "unadjudicated" {
				t.Fatal("unadjudicated route changed on import")
			}
			counts := nativeProjectionCounts(t, f)
			snapshot := nativeSnapshotBytes(t, f, first["id"].(string))
			route := importedFixedRoute()
			test.change(route)
			pull.Runs[0].Findings[0].Route = route
			response := f.call(http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, "import-route-replacement", reviewRequest(1, []PullInput{pull}))
			if !test.valid {
				if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"validation_failed"`) {
					t.Fatalf("malformed route import must fail before writes: status=%d", response.Code)
				}
				if !reflect.DeepEqual(counts, nativeProjectionCounts(t, f)) || nativeSnapshotBytes(t, f, first["id"].(string)) != snapshot {
					t.Fatal("rejected import changed snapshot/domain/event/receipt rows")
				}
				var retained RunDetailResponse
				if string(nativeRead(t, f, "/runs/"+id, &retained)) != string(before) {
					t.Fatal("rejected import changed public retained detail")
				}
				return
			}
			if response.Code != http.StatusCreated {
				t.Fatalf("valid route import refused: status=%d", response.Code)
			}
			var current RunDetailResponse
			nativeRead(t, f, "/runs/"+id, &current)
			if !reflect.DeepEqual(current.Findings[0].Route, route) || current.Run.Routed != 1 || current.Run.Fixed != 0 {
				t.Fatal("valid receiver proof lost or credited as source proof")
			}
			if nativeSnapshotBytes(t, f, first["id"].(string)) != snapshot {
				t.Fatal("replacement rewrote original imported snapshot")
			}
		})
	}
}

func importedRoutedRun() RunInput {
	run := nativeRun("imported-receiver-proof")
	run.Accepted, run.Routed = 0, 1
	finding := &run.Findings[0]
	finding.Disposition = "routed"
	finding.FixSHA, finding.ProofNote, finding.ResolvedAt = "", "", ""
	return run
}

func importedFixedRoute() *FindingRoute {
	route := nativeRouteFixture(0)
	route.Revision = 3
	route.Disposition, route.Reason, route.Actor = "accepted", "Receiver accepted", "receiving-actor_1"
	route.DecidedAt = "2026-01-01T00:05:00Z"
	route.FixSHA, route.ProofNote, route.FixedAt = strings.Repeat("d", 40), "Receiver fix proof", "2026-01-01T00:06:00Z"
	return route
}

func TestImportedFindingRoutesRejectMalformedRetainedReceiverProof(t *testing.T) {
	f := newAPIFixture(t)
	run := importedRoutedRun()
	run.Findings[0].Route = importedFixedRoute()
	pull := nativePull(1)
	pull.Runs = []RunInput{run}
	first := f.observe(t, reviewRequest(0, []PullInput{pull}), "import-fixed-receiver")
	id := runOpaqueID(f.project, "test-import", run.SourceRunID)
	var original RunDetailResponse
	nativeRead(t, f, "/runs/"+id, &original)
	for _, mode := range []string{"superseded_fixed", "actor_spaces"} {
		t.Run(mode, func(t *testing.T) {
			route := importedFixedRoute()
			if mode == "superseded_fixed" {
				route.State = "superseded"
			} else {
				route.Actor = "receiving actor"
			}
			// This SQL represents corrupt retained data, not a supported import.
			if _, err := f.store.DB().Exec(`UPDATE review_findings SET route_json=? WHERE project_id=? AND snapshot_id=? AND run_id=?`, mustJSON(route), f.project, first["id"], id); err != nil {
				t.Fatal(err)
			}
			counts := nativeProjectionCounts(t, f)
			response := f.call(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/runs/"+id, f.readToken, "", nil)
			if response.Code != 500 || !strings.Contains(response.Body.String(), `"code":"storage_unavailable"`) {
				t.Fatalf("malformed retained receiver proof must fail closed: status=%d", response.Code)
			}
			if !reflect.DeepEqual(counts, nativeProjectionCounts(t, f)) {
				t.Fatal("failed read wrote durable rows")
			}
		})
	}
}
