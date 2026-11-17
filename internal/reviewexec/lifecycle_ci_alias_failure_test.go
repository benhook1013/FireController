package reviewexec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/githubhook"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestLifecycleCICollectionFailurePreservesOnlyCurrentCommitAlias(t *testing.T) {
	for _, changed := range []string{"head", "base_sha", "base_ref"} {
		t.Run(changed, func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			attachLifecycleHints(t, f, s)
			pull := lifecyclePull(2)
			r := &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { return pull, nil }}}
			readers := map[string]TargetReader{"test-capability": r}
			lifecycleTick(t, f, s, r)
			key := lifecycleKey{attemptProject, 2}
			due := s.due[key]
			// The retained test-merge alias differs from the source head, and remains
			// scoped to the last successfully observed head/base comparison.
			alias := strings.Repeat("f", 40)
			due.ciCommit = alias
			s.due[key] = due
			hint := githubhook.Hint{RepositoryID: 801, HeadSHA: alias, CI: true}
			hintLifecycle(t, f, hint)
			r.ci = func(context.Context, reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				return reviewprovider.CIObservation{}, errors.New("temporary CI failure")
			}
			target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
			if err != nil || target == nil || !target.forceCI {
				t.Fatal(target, err)
			}
			if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil {
				t.Fatal(err)
			}
			if got := s.due[key].ciCommit; got != alias {
				t.Fatal("failed same-comparison collection discarded known commit alias", got)
			}
			if got := lifecycleRead(t, f).CIState; got != "unknown" {
				t.Fatal("retained alias incorrectly preserved CI success", got)
			}
			wanted := s.due[key].hints[lifecycleEnrichment].wanted
			hintLifecycle(t, f, hint)
			if got := s.due[key].hints[lifecycleEnrichment].wanted; got != wanted+1 {
				t.Fatal("known alias did not retain webhook wake eligibility", got, wanted)
			}
			if retry, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment); err != nil || retry != nil {
				t.Fatal("retained alias bypassed provider backoff", retry, err)
			}
			clock.Add(time.Minute)
			switch changed {
			case "head":
				pull.HeadSHA = strings.Repeat("c", 40)
			case "base_sha":
				pull.BaseSHA = strings.Repeat("d", 40)
			case "base_ref":
				pull.BaseRef = "next"
			}
			target, err = f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleMetadata)
			if err != nil || target == nil {
				t.Fatal(target, err)
			}
			if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil {
				t.Fatal(err)
			}
			if got := s.due[key].ciCommit; got != "" {
				t.Fatal("changed comparison retained obsolete alias", got)
			}
			wanted = s.due[key].hints[lifecycleEnrichment].wanted
			hintLifecycle(t, f, hint)
			if got := s.due[key].hints[lifecycleEnrichment].wanted; got != wanted {
				t.Fatal("old alias admitted after comparison changed", got, wanted)
			}
		})
	}
}
