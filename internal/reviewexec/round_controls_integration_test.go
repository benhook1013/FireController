package reviewexec

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestRoundIntegrationCloseBetweenPrepareAndAdmitChangesNoJournal(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 3, "guard-grant")
	receipt := f.enqueue(t, attemptProject, "prepared-before-close", "cli", 2, 1)
	a := f.start(t, attemptProject, receipt)
	revision := int64(1)
	if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "close-before-admit", 2, "cli", RoundControlRequest{ExpectedRevision: &revision, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	before := attemptState(t, f, a)
	_, err := f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
	attemptCode(t, err, "review_round_closed")
	if attemptState(t, f, a) != before {
		t.Fatal("closed admission recorded active state or transition")
	}
}

func TestRoundIntegrationRequiredDiscoveryIsSeparateFromObservedCoverage(t *testing.T) {
	f, _, id, _, _ := readinessFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 4, "additional-after-old-coverage")
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	captured, err := f.e.ReadPolicyAssessment(t.Context(), scope, id)
	if err != nil || captured.State != "held" || len(captured.Channels) != 1 || !captured.Channels[0].Covered || !captured.Channels[0].RoundsConfigured || captured.Channels[0].RoundsComplete {
		t.Fatal("old coverage satisfied fresh discovery grant", captured, err)
	}
	observed, err := f.e.ReadSourceReadiness(t.Context(), scope, 2)
	if err != nil || observed.State != "held" || !slices.Contains(observed.Reasons, "review_rounds_pending") {
		t.Fatal("observed readiness omitted round requirement", observed, err)
	}
	revision := int64(1)
	if _, err = f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "stop-discovery", 2, "cli", RoundControlRequest{ExpectedRevision: &revision, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	observed, err = f.e.ReadSourceReadiness(t.Context(), scope, 2)
	if err != nil || observed.State != "ready_as_observed" {
		t.Fatal("closed discovery erased existing valid coverage", observed, err)
	}
	// Later Git changes preserve completed PR progress and explicit closure.
	// The original captured comparison and consumed history remain unchanged.
	changed := SourcePlan{Repository: reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, Pull: reviewprovider.Pull{Number: 2, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("9", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, ParentSHA: strings.Repeat("b", 40), ObservedAt: stamp()}
	state, err := f.e.Comparison(t.Context(), attemptProject, changed, "cli")
	if err != nil || !state.Covered || state.Pending || !state.RoundsComplete {
		t.Fatal("Git change lost completed PR progress", state, err)
	}
	if done, err := f.e.DiscoveryComplete(t.Context(), attemptProject, changed, "cli"); err != nil || !done {
		t.Fatal("changed head silently reopened explicit closure", done, err)
	}
	if state := roundRead(t, f, 2, "cli"); state.Used != 0 || !state.Closed {
		t.Fatal("head movement rewrote allowance", state)
	}
}

func TestRoundIntegrationStrictHTTPRefusesNullAndCloseAllocationFields(t *testing.T) {
	f := roundFixture(t)
	mux := http.NewServeMux()
	f.e.Register(mux)
	for i, body := range []string{
		`{"expected_revision":0,"action":"close","minimum":null}`,
		`{"expected_revision":0,"action":"close","maximum":null}`,
		`{"expected_revision":0,"action":"close","minimum":0}`,
		`{"expected_revision":0,"action":"grant","minimum":0,"maximum":null}`,
		`{"expected_revision":0,"action":"grant","minimum":0,"maximum":1.5}`,
	} {
		before := roundTables(t, f)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+attemptProject+"/reviews/rounds/2/cli", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+f.admin)
		request.Header.Set("X-FC-Instance", f.store.InstanceID())
		request.Header.Set("Idempotency-Key", fmt.Sprintf("invalid-round-%d", i))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || before != roundTables(t, f) {
			t.Fatal("invalid control input changed state", response.Code)
		}
	}
}
