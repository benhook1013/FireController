package reviewprocess

import (
	"slices"
	"time"

	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

// MatchesCacheConfiguration compares the immutable runner construction with the
// current operator configuration. It exposes no environment or credential data.
// Ambient environment changes conservatively require reconstructing the runner.
func (r *Runner) MatchesCacheConfiguration(manager *reviewworkspace.Manager, executable string, authenticationEnvironment map[string]string, allowanceTimeout time.Duration) bool {
	if r == nil || manager == nil || r.manager != manager {
		return false
	}
	var options []Option
	if allowanceTimeout != 0 {
		options = append(options, WithUsageTimeout(allowanceTimeout))
	}
	expected, err := New(manager, executable, authenticationEnvironment, options...)
	if err != nil || expected.executable != r.executable || expected.usageTimeout != r.usageTimeout {
		return false
	}
	actualEnv := append([]string(nil), r.env...)
	expectedEnv := append([]string(nil), expected.env...)
	slices.Sort(actualEnv)
	slices.Sort(expectedEnv)
	return slices.Equal(actualEnv, expectedEnv)
}
