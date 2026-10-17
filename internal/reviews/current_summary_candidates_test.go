package reviews

import (
	"fmt"
	"testing"
)

func TestCurrentSummaryStagedChannelReplacementsRemovalAndTies(t *testing.T) {
	f := newAPIFixture(t)
	pull, other := nativePull(1), nativePull(2)
	channels := []string{"hosted", "cli", "manual", "subagent"}
	for _, channel := range channels {
		for _, suffix := range []string{"first", "second"} {
			pull.Attempts = append(pull.Attempts, currentSummaryAttempt(channel+"-"+suffix, channel, "2026-01-01T00:00:00Z", ""))
		}
	}
	other.Attempts = []Attempt{currentSummaryAttempt("untouched-second-pr", "cli", "2026-01-01T00:00:00Z", "")}
	check := func(suffix string) {
		t.Helper()
		got := assertCurrentSummaryReduction(t, f, 1).Pull.LatestAttempts
		if len(got) != len(channels) {
			t.Fatal("changed candidates lost a supported channel")
		}
		for i, channel := range channels {
			if got[i].Channel != channel || got[i].SourceAttemptID != channel+"-"+suffix {
				t.Fatal("staged channel selection or position tie changed", channel, got[i])
			}
		}
		otherSummary := assertCurrentSummaryReduction(t, f, 2)
		if len(otherSummary.Pull.LatestAttempts) != 1 || otherSummary.Pull.LatestAttempts[0].SourceAttemptID != "untouched-second-pr" {
			t.Fatal("scoped staged candidates changed another PR")
		}
	}
	f.observe(t, reviewRequest(0, []PullInput{pull, other}), "all-channel-initial-ties")
	check("second")
	for i := 0; i < len(pull.Attempts); i += 2 {
		pull.Attempts[i].FinishedAt = "2026-01-01T01:00:00Z"
	}
	f.observe(t, reviewRequest(1, []PullInput{pull, other}), "all-channel-replacements")
	check("first")
	removed := pull.Attempts
	pull.Attempts = nil
	for i := 1; i < len(removed); i += 2 {
		pull.Attempts = append(pull.Attempts, removed[i])
	}
	f.observe(t, reviewRequest(2, []PullInput{pull, other}), "all-channel-winner-removal")
	check("second")
	// Active finish corrections and returning membership may regain a later
	// position. Immutable producer/PR/channel/head and start times stay fixed.
	for i, channel := range channels {
		a := removed[2*i]
		a.FinishedAt = ""
		pull.Attempts = append(pull.Attempts, a)
		if a.SourceAttemptID != fmt.Sprintf("%s-first", channel) {
			t.Fatal("fixture order changed")
		}
	}
	f.observe(t, reviewRequest(3, []PullInput{pull, other}), "all-channel-return-ties")
	check("first")
}
