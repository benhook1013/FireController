package reviewprovider

import (
	"strings"
	"testing"
)

func TestOptionalAttentionReusesSameObservationDiscovery(t *testing.T) {
	f := ciWorkflowFailureFixture(t, "pending")
	for pass := 1; pass <= 2; pass++ {
		got, failures, err := f.g.ObserveCIGateWithWorkflows(t.Context(), ciObservationTarget(), "main", []int64{31})
		if err != nil || got.State != "pending" || len(failures) != 1 || failures[0].RunID != 60 || failures[0].JobID != 90 {
			t.Fatal(got.State, failures, err)
		}
		paths, counts := f.snapshot()
		suite := 0
		for _, p := range paths {
			if strings.Contains(p, "check_suite_id=") {
				suite++
			}
		}
		t.Logf("optional attention total=%d suite-discovery=%d full-and-suite-head-list=%d direct60=%d jobs60=%d", len(paths), suite, counts[actionsRunsPath], counts[actionsRunsPath+"/60"], counts[actionsRunsPath+"/60/attempts/2/jobs"])
		if suite != 0 || len(paths) != 17*pass-(pass-1) || counts[actionsRunsPath] != pass || counts[actionsRunsPath+"/60"] != 2*pass || counts[actionsRunsPath+"/60/attempts/2/jobs"] != pass {
			t.Fatalf("redundant suite count %d", suite)
		}
	}
}
