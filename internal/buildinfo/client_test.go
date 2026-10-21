package buildinfo

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestExecutableSourceDoesNotInventReleaseProvenance(t *testing.T) {
	commit := strings.Repeat("a", 40)
	for _, test := range []struct {
		settings        []debug.BuildSetting
		revision, state string
	}{
		{nil, "unknown", "unknown"},
		{[]debug.BuildSetting{{Key: "vcs.modified", Value: "false"}}, "unknown", "unknown"},
		{[]debug.BuildSetting{{Key: "vcs.revision", Value: commit}, {Key: "vcs.modified", Value: "false"}}, commit, "clean"},
		{[]debug.BuildSetting{{Key: "vcs.revision", Value: commit}, {Key: "vcs.modified", Value: "true"}}, commit, "modified"},
		{[]debug.BuildSetting{{Key: "vcs.revision", Value: commit}}, commit, "unknown"},
		{[]debug.BuildSetting{{Key: "vcs.revision", Value: "bad revision"}, {Key: "vcs.modified", Value: "false"}}, "unknown", "unknown"},
	} {
		r, s := source(test.settings)
		if r != test.revision || s != test.state {
			t.Fatalf("source=%q/%q expected=%q/%q", r, s, test.revision, test.state)
		}
	}
}
