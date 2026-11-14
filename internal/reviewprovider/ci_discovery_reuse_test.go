package reviewprovider

import (
	"strings"
	"testing"
)

func TestCIObservationReusesOnlySameObservationDiscovery(t *testing.T) {
	f := newCIObservationFixture(t, "merge")
	for pass := 1; pass <= 2; pass++ {
		got, err := f.g.ObserveRequiredCI(t.Context(), ciObservationTarget())
		if err != nil || got.State != "satisfied" {
			t.Fatal(got, err)
		}
		paths, counts := f.snapshot()
		for _, path := range paths {
			if strings.Contains(path, "check_suite_id=") {
				t.Fatalf("redundant suite discovery: %s", path)
			}
		}
		// Each observation still freshly reads the complete head inventory twice,
		// direct current run twice and exact-attempt jobs once. No cross-call reuse.
		if counts[actionsRunsPath] != 2*pass || counts[actionsRunsPath+"/50"] != 2*pass || counts[actionsRunsPath+"/50/attempts/2/jobs"] != pass {
			t.Fatalf("fresh proof reads changed: %#v", counts)
		}
		t.Logf("pass=%d total HTTP requests=%d Actions discovery=%d", pass, len(paths), counts[actionsRunsPath])
	}
}
