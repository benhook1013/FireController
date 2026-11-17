package reviews

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Use the production handlers, including the explicit browser display opt-in.
func currentFindingsDisplayRead(t *testing.T, f *apiFixture, route string, dst any) []byte {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews"+route, nil)
	r.Header.Set("Authorization", "Bearer "+f.readToken)
	r.Header.Set(displayStartHeader, "v1")
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), dst) != nil {
		t.Fatalf("display read %s: %d %s", route, w.Code, w.Body.String())
	}
	return append([]byte(nil), w.Body.Bytes()...)
}

func assertCurrentFindingCounts(t *testing.T, run RunSummary, accepted, pending int64) {
	t.Helper()
	if run.CurrentAccepted == nil || *run.CurrentAccepted != accepted || run.CurrentPendingFixes == nil || *run.CurrentPendingFixes != pending {
		t.Fatalf("wrong current obligations: %+v; want accepted=%d pending=%d", run, accepted, pending)
	}
}

func TestCurrentFindingsDisplayPostFinalizationCorrectionAndSnapshotScope(t *testing.T) {
	f := newAPIFixture(t)
	otherPullRun := nativeRun("other-pull")
	mustNativePublish(t, f, NativePublication{Pull: nativePull(2), Run: &otherPullRun})
	run := nativeUndecidedRun("frozen-three", 3)
	mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
	input := nativeAdjudicationInput(run)
	for i := range input.Findings {
		decideNativeFinding(&input.Findings[i], "accepted")
	}
	input.Finalized, input.FrozenCounts = true, &NativeCounts{Found: 3, Accepted: 3}
	frozen, changed, err := applyNativeAdjudication(f, input)
	if err != nil || !changed {
		t.Fatal("finalization failed", changed, err)
	}
	id := runOpaqueID(f.project, nativeSource, run.SourceRunID)
	routes := []string{"/queue", "/queue?view=recent", "/pulls/1", "/pulls/1/runs", "/pulls/1/runs?view=timeline", "/runs/" + id}
	pinned := make(map[string][]byte)
	for _, route := range routes {
		if strings.HasPrefix(route, "/queue") {
			separator := "?"
			if strings.Contains(route, "?") {
				separator = "&"
			}
			var first QueueResponse
			currentFindingsDisplayRead(t, f, route+separator+"limit=1", &first)
			if !first.HasMore || first.Cursor == "" {
				t.Fatal("missing pinned queue continuation", first)
			}
			selected := route + separator + "limit=1&cursor=" + url.QueryEscape(first.Cursor)
			var value any
			pinned[selected] = currentFindingsDisplayRead(t, f, selected, &value)
			continue
		}
		separator := "?"
		if strings.Contains(route, "?") {
			separator = "&"
		}
		selected := route + separator + "snapshot=" + frozen.ID
		var value any
		pinned[selected] = currentFindingsDisplayRead(t, f, selected, &value)
	}
	// An identical source identity in another project must not affect this one.
	if _, err := f.store.DB().Exec(`INSERT INTO projects VALUES('current-findings-other','Other',702,'owner/other')`); err != nil {
		t.Fatal(err)
	}
	other := *f
	other.project = "current-findings-other"
	otherRun := nativeRun(run.SourceRunID)
	otherReceipt := mustNativePublish(t, &other, NativePublication{Pull: nativePull(1), Run: &otherRun})
	otherDigest := nativeSnapshotBytes(t, &other, otherReceipt.ID)
	for i := 0; i < 2; i++ {
		input.Findings[i].FixSHA = strings.Repeat("c", 40)
		input.Findings[i].ProofNote = "Current retained fix"
		input.Findings[i].ResolvedAt = "2026-01-01T00:05:00Z"
	}
	decideNativeFinding(&input.Findings[2], "rejected")
	if _, changed, err := applyNativeAdjudication(f, input); err != nil || !changed {
		t.Fatal("correction failed", changed, err)
	}
	for _, route := range routes {
		var got RunSummary
		switch {
		case strings.HasPrefix(route, "/queue"):
			var value QueueResponse
			currentFindingsDisplayRead(t, f, route, &value)
			if len(value.Items) != 2 {
				t.Fatalf("unexpected queue: %+v", value)
			}
			for _, pull := range value.Items {
				if pull.Number == 1 {
					got = pull.RecentRuns[0]
				}
			}
		case route == "/pulls/1":
			var value PullResponse
			currentFindingsDisplayRead(t, f, route, &value)
			got = value.Pull.RecentRuns[0]
		case strings.HasPrefix(route, "/pulls/1/runs"):
			var value RunsResponse
			currentFindingsDisplayRead(t, f, route, &value)
			got = value.Items[0]
		default:
			var value RunDetailResponse
			currentFindingsDisplayRead(t, f, route, &value)
			got = value.Run
		}
		assertCurrentFindingCounts(t, got, 2, 0)
		if got.Found != 3 || got.Accepted != 3 || got.Routed != 0 || got.Fixed != 2 || got.StartedAt != run.StartedAt || got.FinishedAt != run.FinishedAt {
			t.Fatalf("%s changed historical counts/time or current proof: %+v", route, got)
		}
	}
	// A new current acceptance is a real pending obligation despite finalization.
	decideNativeFinding(&input.Findings[2], "accepted")
	if _, changed, err := applyNativeAdjudication(f, input); err != nil || !changed {
		t.Fatal("acceptance failed", changed, err)
	}
	var current RunDetailResponse
	currentFindingsDisplayRead(t, f, "/runs/"+id, &current)
	assertCurrentFindingCounts(t, current.Run, 3, 1)
	for route, before := range pinned {
		var value any
		if after := currentFindingsDisplayRead(t, f, route, &value); !bytes.Equal(before, after) {
			t.Fatalf("pinned response changed: %s", route)
		}
	}
	if nativeSnapshotBytes(t, &other, otherReceipt.ID) != otherDigest {
		t.Fatal("other project's retained source changed")
	}
	var ordinary RunDetailResponse
	body := nativeRead(t, f, "/runs/"+id, &ordinary)
	if bytes.Contains(body, []byte("current_accepted")) || bytes.Contains(body, []byte("current_pending_fixes")) {
		t.Fatal("display projection leaked into ordinary read", string(body))
	}
}

func TestCurrentFindingsDisplayExternalAndUnknownInventory(t *testing.T) {
	f := newAPIFixture(t)
	complete := nativeRun("external-complete")
	complete.Findings[0].FixSHA, complete.Findings[0].ProofNote, complete.Findings[0].ResolvedAt = "", "", ""
	incomplete := nativeRun("external-missing")
	incomplete.Complete = false
	incomplete.Findings = nil
	zero := nativeUndecidedRun("external-zero", 0)
	zero.Finalized = true
	p := nativePull(1)
	p.Runs = []RunInput{complete, incomplete, zero}
	f.observe(t, reviewRequest(0, []PullInput{p}), "external-inventory")
	for _, route := range []string{"/pulls/1/runs", "/pulls/1/runs?view=timeline"} {
		var value RunsResponse
		currentFindingsDisplayRead(t, f, route, &value)
		for _, run := range value.Items {
			switch run.SourceRunID {
			case complete.SourceRunID:
				assertCurrentFindingCounts(t, run, 1, 1)
			case zero.SourceRunID:
				assertCurrentFindingCounts(t, run, 0, 0)
			case incomplete.SourceRunID:
				if run.CurrentAccepted != nil || run.CurrentPendingFixes != nil {
					t.Fatal("missing external inventory invented obligations", run)
				}
			default:
				t.Fatal("unexpected run", run)
			}
		}
	}
}

func TestCurrentFindingsDisplayPartialRetainedInventory(t *testing.T) {
	f := newAPIFixture(t)
	run := nativeUndecidedRun("partial-retained", 1)
	run.Outcome, run.Complete, run.Attributable = "failed", false, false
	run.Found = 2 // Provider total exceeds the one retained finding.
	run.Findings[0].Disposition = "accepted"
	run.Findings[0].DecisionReason = "Retained decision"
	run.Findings[0].Actor = "adjudicator"
	run.Findings[0].DecidedAt = "2026-01-01T00:04:00Z"
	run.Accepted = 1
	mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
	var pull PullResponse
	currentFindingsDisplayRead(t, f, "/pulls/1", &pull)
	compact := pull.Pull.RecentRuns[0]
	assertCurrentFindingCounts(t, compact, 1, 1)
	if compact.RetainedUndecided != nil {
		t.Fatal("compact partial undecided semantics changed")
	}
	var timeline RunsResponse
	currentFindingsDisplayRead(t, f, "/pulls/1/runs?view=timeline", &timeline)
	assertCurrentFindingCounts(t, timeline.Items[0], 1, 1)
	if timeline.Items[0].RetainedUndecided == nil || *timeline.Items[0].RetainedUndecided != 0 || timeline.Items[0].Complete {
		t.Fatal("partial evidence acquired complete credit", timeline.Items[0])
	}
}
