package reviews

import "testing"

func TestCompactPartialInventoryRemainsUnknownUntilTimelineRead(t *testing.T) {
	f := newAPIFixture(t)
	run := nativeUndecidedRun("compact-partial", 1)
	run.Outcome, run.Complete, run.Attributable = "failed", false, false
	mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
	var pull PullResponse
	nativeRead(t, f, "/pulls/1", &pull)
	if len(pull.Pull.RecentRuns) != 1 || pull.Pull.RecentRuns[0].RetainedUndecided != nil {
		t.Fatal("compact summary must not claim an inventory it did not read")
	}
	var timeline RunsResponse
	nativeRead(t, f, "/pulls/1/runs?view=timeline&limit=10", &timeline)
	if len(timeline.Items) != 1 || timeline.Items[0].RetainedUndecided == nil || *timeline.Items[0].RetainedUndecided != 1 {
		t.Fatal("timeline must expose the actual retained undecided inventory")
	}
}
