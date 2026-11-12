package reviewexec

import (
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/githubhook"
)

func TestLifecycleHintInvalidConfigurationIsRepositoryScoped(t *testing.T) {
	for name, body := range map[string]string{
		"oversized":     strings.Repeat(" ", 8193),
		"invalid_json":  `{"queue":`,
		"unknown_field": `{"unexpected":true}`,
		"invalid_shape": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			f, s, _ := lifecycleFixture(t)
			attachLifecycleHints(t, f, s)
			f.configure(t, otherAttemptProject, "test-capability", 0, true)
			admissionSQL(t, f, `UPDATE review_execution_config SET body_json=? WHERE project_id=?`, body, otherAttemptProject)
			before := lifecycleHintCounts(t, f)
			hint := githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{2}, Metadata: true, CI: true, HeadSHA: lifecyclePull(2).HeadSHA}
			if err := f.e.HintGitHub(t.Context(), hint); err != nil {
				t.Fatal("invalid unrelated project blocked healthy repository", err)
			}
			key := lifecycleKey{attemptProject, 2}
			due := s.due[key]
			if len(s.due) != 1 || due.hints[lifecycleMetadata].wanted != 1 || due.hints[lifecycleEnrichment].wanted != 1 {
				t.Fatal("healthy demand missing or invalid project admitted", s.due)
			}
			hint.RepositoryID = 802
			if err := f.e.HintGitHub(t.Context(), hint); !lifecycleHintOverloaded(err) {
				t.Fatal("invalid target configuration was admitted", err)
			}
			if len(s.due) != 1 || s.due[key] != due || !reflect.DeepEqual(before, lifecycleHintCounts(t, f)) {
				t.Fatal("invalid target changed healthy demand or durable state")
			}
		})
	}
}
