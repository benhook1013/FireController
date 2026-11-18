package reviewruntime

import "github.com/benhook1013/FireController/internal/reviewexec"

// Retained waits retry the same comparison, not newly discovered source.
func sameHostedRetryComparison(baseline, current reviewexec.Anchor) bool {
	baseline.ObservedAt = ""
	current.ObservedAt = ""
	baseline.Repository = ""
	current.Repository = ""
	return baseline == current
}
