package reviewexec

import (
	"github.com/benhook1013/FireController/internal/githubhook"
	"testing"
)

func TestLifecycleExistingHintSurvivesOrdinarySchedulingCapacity(t *testing.T) {
	f, s, _ := lifecycleFixture(t)
	attachLifecycleHints(t, f, s)
	key := lifecycleKey{attemptProject, 2}
	s.due[key] = lifecycleDue{}
	for n := range 64 * 100 {
		s.due[lifecycleKey{"other", int64(n + 1)}] = lifecycleDue{}
	}
	err := f.e.HintGitHub(t.Context(), githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{2}, Metadata: true})
	if err != nil {
		t.Fatal("ordinary scheduled entries rejected existing hint", err)
	}
	if s.due[key].hints[lifecycleMetadata].wanted != 1 {
		t.Fatal("existing demand missing")
	}
}

func TestLifecycleHintRejectsMissingSelectedReader(t *testing.T) {
	f, s, _ := lifecycleFixture(t)
	attachLifecycleHints(t, f, s)
	s.readers = nil
	err := f.e.HintGitHub(t.Context(), githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{2}, Metadata: true})
	if !lifecycleHintOverloaded(err) {
		t.Fatal("missing reader accepted demand", err)
	}
	if len(s.due) != 0 {
		t.Fatal("missing reader recorded unconsumable hints", s.due)
	}
}
