package reviewexec

import (
	"strings"
	"testing"
)

func TestFrontFrameRejectsObservedParentCycle(t *testing.T) {
	const observed = "2026-11-20T19:00:00Z"
	var revision int64
	config := Configuration{ExpectedRevision: &revision, CapabilityID: "test", Mode: "stacked", Queue: []int64{2, 4, 6}, Channels: []string{"cli"}}
	head2, head4, head6 := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	pull := func(n, parent int64, head, base string) frontPull {
		return frontPull{Number: n, ParentPullNumber: parent, HeadSHA: head, BaseSHA: base, ParentSHA: base, State: "open", BaseRef: "main", ObservedAt: observed}
	}
	frame := frontFrame{repository: 1, configuration: 1, checkedAt: observed, observedAt: observed, sourceReason: "source_unavailable",
		prefix: []frontPull{pull(2, 4, head2, head4), pull(4, 6, head4, head6)}}
	if !validFrontFrame(frame, config, "cli") {
		t.Fatal("unobserved parent in the unacquired suffix rejected")
	}
	frame.prefix[1] = pull(4, 2, head4, head2)
	if validFrontFrame(frame, config, "cli") {
		t.Fatal("reciprocal observed parents accepted")
	}
}
