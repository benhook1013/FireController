package reviewexec

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func runRouteSetup(t *testing.T) (*attemptFixture, string) {
	t.Helper()
	f, source, sourceID := routeFixture(t, 1)
	adjudicationComplete(t, f, source)
	routePolicyReceiver(t, f, sourceID)
	if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,'selection-worker','Selector')`, attemptProject); err != nil {
		t.Fatal(err)
	}
	return f, sourceID
}

func runRouteQueue(t *testing.T, f *attemptFixture, queue []int64) {
	t.Helper()
	revision := int64(2)
	if err := f.store.DB().QueryRow(`SELECT MAX(revision) FROM review_execution_config WHERE project_id=?`, attemptProject).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), fmt.Sprintf("selection-route-queue-%d", revision), Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "independent", Queue: queue, Channels: []string{"cli", "hosted"}}); err != nil {
		t.Fatal(err)
	}
}

func TestRunSelectionReceivingRoutesHoldCoveredOrClosedFront(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{false: "covered", true: "closed"}[closed], func(t *testing.T) {
			f, sourceID := runRouteSetup(t)
			route := routeCreate(t, f, sourceID, "incoming-selection-route", routeRequest(0, 0, 4))
			// The routed source is outside the selected queue. Its receiving
			// obligation still belongs to the covered/finished front.
			runRouteQueue(t, f, []int64{4})
			if closed {
				revision := int64(0)
				if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "close-receiver", 4, "cli", RoundControlRequest{ExpectedRevision: &revision, Action: "close"}); err != nil {
					t.Fatal(err)
				}
			}
			for i, key := range []string{"route-undecided", "route-accepted"} {
				if i == 1 {
					routeDecision(t, f, route.ID, "accept-receiving-route", 1, "accepted")
				}
				r, _ := selectionEnqueue(t, f, key, "cli", nil)
				reader := &refreshTestReader{}
				selectionNext(t, f, reader)
				got := selectionRead(t, f, r.ID)
				if got.State != "held" || got.Reason != "routed_findings_pending" || got.OperationID != "" || reader.calls.Load() != 2 {
					t.Fatal("receiving obligation was skipped", got, reader.calls.Load())
				}
			}
			routeFix(t, f, route.ID, "receiving-route-fixed", 2, "", strings.Repeat("d", 40))
			// Once the target's own route is fixed, include independent later work.
			runRouteQueue(t, f, []int64{4, 6})
			r, _ := selectionEnqueue(t, f, "after-receiving-fix", "cli", nil)
			reader := &refreshTestReader{}
			selectionNext(t, f, reader)
			got := selectionRead(t, f, r.ID)
			if got.State != "selected" || got.OperationID == "" {
				t.Fatal("fixed receiving obligation still blocked", got)
			}
			op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), got.OperationID)
			if err != nil || op.PullNumber != 6 || reader.calls.Load() != 4 {
				t.Fatal("did not advance exactly to suffix", op, err, reader.calls.Load())
			}
		})
	}
}

func TestRunSelectionReceivingRouteArrivingDuringSourcePreventsChild(t *testing.T) {
	f, sourceID := runRouteSetup(t)
	runRouteQueue(t, f, []int64{2, 4, 6})
	r, _ := selectionEnqueue(t, f, "late-receiving-route", "cli", nil)
	var routeOnce sync.Once
	reader := &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		p := reviewprovider.Pull{Number: n, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}
		if n == 2 {
			p.State, p.Merged = "closed", true
		}
		if n == 6 {
			routeOnce.Do(func() { routeCreate(t, f, sourceID, "late-route-write", routeRequest(0, 0, 6)) })
		}
		return p, nil
	}}
	var before int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_operations`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	selectionNext(t, f, reader)
	got := selectionRead(t, f, r.ID)
	var after int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_operations`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if got.State != "held" || got.Reason != "routed_findings_pending" || got.OperationID != "" || after != before || reader.calls.Load() != 5 {
		t.Fatal("final selection transaction missed incoming route", got, before, after, reader.calls.Load())
	}
}
