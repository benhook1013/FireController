package reviewexec

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func pendingRouteHold(t *testing.T, n int) (*attemptFixture, Attempt, string, []string) {
	t.Helper()
	f, prior, evidence := routeFixture(t, n)
	routes := []string{}
	for i := 0; i < n; i++ {
		r := routeCreate(t, f, evidence, "pending-route-"+string(rune('a'+i)), routeRequest(int64(i), int64(i), 4))
		routes = append(routes, r.ID)
		routeDecision(t, f, r.ID, "accept-pending-"+string(rune('a'+i)), 1, "accepted")
	}
	coverageFinalize(t, f, prior, evidence, int64(n))
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "held-source", "cli", 2, 2))
	_, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", "review_evidence_pending", "")
	if err != nil {
		t.Fatal(err)
	}
	return f, a, evidence, routes
}
func TestPendingEvidenceIdentifiesOutgoingRouteAndCurrentResolution(t *testing.T) {
	f, a, evidence, routes := pendingRouteHold(t, 1)
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	before := routeState(t, f, a)
	v, err := f.e.readPendingEvidence(t.Context(), scope, a.OperationID)
	if err != nil || v == nil || len(v.Items) != 1 || v.HasMore {
		t.Fatal(v, err)
	}
	x := v.Items[0]
	if x.EvidenceID != evidence || x.Finding == nil || *x.Finding != 0 || x.RouteID != routes[0] || x.TargetPullNumber != 4 || x.Reason != "route_fix_pending" {
		t.Fatal("missing exact blocking reference", x)
	}
	if routeState(t, f, a) != before {
		t.Fatal("blocker read mutated authority")
	}
	routeFix(t, f, routes[0], "resolve-pending", 2, "", strings.Repeat("9", 40))
	v, err = f.e.readPendingEvidence(t.Context(), scope, a.OperationID)
	if err != nil || v == nil || len(v.Items) != 0 || v.HasMore {
		t.Fatal("resolved blocker retained", v, err)
	}
	held, err := f.e.ReadOperation(t.Context(), scope, a.OperationID)
	if err != nil || held.State != "held" || held.Reason != "review_evidence_pending" {
		t.Fatal("current read resumed original hold", held, err)
	}
	if _, err = f.e.readPendingEvidence(t.Context(), evidenceReadScope(t, f, otherAttemptProject, f.other), a.OperationID); err == nil {
		t.Fatal("cross-project read allowed")
	}
}
func TestPendingEvidenceBoundedAndDoesNotRepeatPrivateFindingText(t *testing.T) {
	f, a, _, _ := pendingRouteHold(t, 6)
	v, err := f.e.readPendingEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), a.OperationID)
	if err != nil || len(v.Items) != 5 || !v.HasMore {
		t.Fatal(v, err)
	}
	b, _ := json.Marshal(v)
	if len(b) > 4096 || strings.Contains(string(b), "Explicit receiving reason") || strings.Contains(string(b), "script>literal") {
		t.Fatal("unbounded or repeated private text", string(b))
	}
}
func TestPendingEvidenceHTTPExpansionIsExplicitAndLocal(t *testing.T) {
	f, a, _, _ := pendingRouteHold(t, 1)
	for _, suffix := range []string{"", "/status"} {
		path := "/api/v1/projects/" + attemptProject + "/reviews/operations/" + a.OperationID + suffix
		old := f.request("GET", path, f.reader, "", "")
		if old.Code != 200 || strings.Contains(old.Body.String(), "pending_evidence") {
			t.Fatal("ordinary response changed", old.Code, old.Body.String())
		}
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+f.reader)
		req.Header.Set(pendingEvidenceHeader, "true")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, req)
		var body map[string]json.RawMessage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["pending_evidence"] == nil {
			t.Fatal("expanded response missing", w.Code, w.Body.String())
		}
		var prior map[string]json.RawMessage
		json.Unmarshal(old.Body.Bytes(), &prior)
		delete(body, "pending_evidence")
		if !reflect.DeepEqual(body, prior) {
			t.Fatal("expansion changed original state")
		}
	}
}

func TestPendingEvidenceNamesTheRequiredActionAsAdjudicationProgresses(t *testing.T) {
	f, prior, evidence, _ := coverageFixture(t, 1, false)
	adjudicationComplete(t, f, prior)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "held-progress", "cli", 2, 1))
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", "review_evidence_pending", ""); err != nil {
		t.Fatal(err)
	}
	check := func(reasons ...string) {
		t.Helper()
		v, err := f.e.readPendingEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), a.OperationID)
		if err != nil || v == nil || len(v.Items) != len(reasons) {
			t.Fatal(v, err)
		}
		for i, reason := range reasons {
			if v.Items[i].EvidenceID != evidence || v.Items[i].Reason != reason {
				t.Fatal("incorrect next action", v.Items)
			}
		}
	}
	check("finding_undecided")
	adjudicationDecide(t, f, evidence, "accept-progress", 0, 0, "accepted")
	check("fix_pending", "finalization_pending")
	coverageFix(t, f, evidence, "fix-progress", 1, 0, "")
	check("finalization_pending")
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "finalize-progress", evidence, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(2)})
	adjudicationReceipt(t, body, err, evidence, "finalized", 3)
	check()
}

func TestPendingEvidenceRejectsDirtyOrCorruptSelectedRecords(t *testing.T) {
	for _, statement := range []string{
		`UPDATE review_comparison_proofs SET state='dirty' WHERE state='pending'`,
		`UPDATE review_execution_evidence SET body_json='{}'`,
	} {
		t.Run(statement, func(t *testing.T) {
			f, a, _, _ := pendingRouteHold(t, 1)
			if _, err := f.store.DB().Exec(statement); err != nil {
				t.Fatal(err)
			}
			if _, err := f.e.readPendingEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), a.OperationID); err == nil {
				t.Fatal("corrupt evidence looked clear")
			}
		})
	}
}
