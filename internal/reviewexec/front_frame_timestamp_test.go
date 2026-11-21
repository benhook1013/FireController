package reviewexec

import (
	"strings"
	"testing"
)

func TestFrontFrameCheckedAtCoversLatestObservation(t *testing.T) {
	const earlier = "2026-11-20T19:00:00Z"
	const latest = "2026-11-20T19:00:01Z"
	const later = "2026-11-20T19:00:02Z"
	const equivalent = "2026-11-21T08:00:01+13:00"

	expectedRevision := int64(0)
	cfg := Configuration{
		ExpectedRevision: &expectedRevision,
		CapabilityID:     "test",
		Mode:             "independent",
		Queue:            []int64{1, 2},
		Channels:         []string{"cli"},
	}
	pull := frontPull{
		Number: 1, State: "open",
		HeadSHA:   strings.Repeat("a", 40),
		BaseSHA:   strings.Repeat("b", 40),
		ParentSHA: strings.Repeat("b", 40),
		BaseRef:   "main", ObservedAt: latest,
	}
	second := pull
	second.Number, second.ObservedAt = 2, earlier
	frame := frontFrame{
		repository: 1, configuration: 1, complete: true,
		observedAt: latest, prefix: []frontPull{pull, second},
	}
	for _, tc := range []struct {
		name, checkedAt string
		valid           bool
	}{
		{"earlier than the latest observation", earlier, false},
		{"equal to the latest observation", latest, true},
		{"later than the latest observation", later, true},
		{"same instant with a different offset", equivalent, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := frame
			copy.checkedAt = tc.checkedAt
			if got := validFrontFrame(copy, cfg, "cli"); got != tc.valid {
				t.Fatalf("validFrontFrame = %t, want %t", got, tc.valid)
			}
		})
	}

	frame.complete = false
	frame.sourceReason = "source_unavailable"
	frame.prefix = []frontPull{}
	frame.observedAt = ""
	frame.checkedAt = earlier
	if !validFrontFrame(frame, cfg, "cli") {
		t.Fatal("empty incomplete prefix must not require an observation")
	}
}
