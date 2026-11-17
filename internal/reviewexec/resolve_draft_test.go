package reviewexec_test

import (
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestResolveDraftRefusesExecutionButAllowsSourceObservation(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			reader, config, request := sourceFixture()
			request.Channel, request.AllowOutOfOrder = channel, true
			prepared, err := reviewexec.ResolveSource(t.Context(), reader, 701, config, request)
			if err != nil {
				t.Fatal(err)
			}
			draft := true
			pull := reader.pulls[request.PullNumber]
			pull.IsDraft = &draft
			reader.pulls[pull.Number] = pull
			plan, err := reviewexec.ResolveSource(t.Context(), reader, 701, config, request)
			requireSourceError(t, plan, err, "review_held", "pull_draft")
			err = reviewexec.RecheckSource(t.Context(), reader, 701, config, request, prepared)
			requireSourceError(t, reviewexec.SourcePlan{}, err, "review_held", "pull_draft")
			config.Paused = true
			observed, err := reviewexec.ResolveSourceObservation(t.Context(), reader, 701, config, pull.Number, pull.HeadSHA)
			if err != nil || observed.Pull.IsDraft == nil || !*observed.Pull.IsDraft || observed.Pull.HeadSHA != prepared.Pull.HeadSHA {
				t.Fatal("draft source observation was disabled or rebound", observed, err)
			}
			config.Paused, draft = false, false
			if _, err := reviewexec.ResolveSource(t.Context(), reader, 701, config, request); err != nil {
				t.Fatal("ready PR still held", err)
			}
		})
	}
}
