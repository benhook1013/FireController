package reviewexec

import (
	"strings"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// Called only after the normal fresh observation and typed inventory validation.
// Unknown required entries cannot hide an independently proven failure, but a
// collector coherence downgrade must keep every retained count informational.
// The canonical retained assessment and its pass rules remain unchanged.
func ciWatchRequiredFailure(o reviewprovider.CIObservation) bool {
	if o.State != "unproven" || o.Reason != "ci_required_checks_unproven" || o.Assessment.State != o.State || o.Assessment.Reason != o.Reason || o.Selection == "" || o.Results == nil || o.Actions == nil || o.Attempts == nil {
		return false
	}
	// Unproven observations may retain incomplete candidate facts. Require the
	// same complete comparison binding used for ordinary non-unproven results.
	c, target := o.Candidate, o.Target
	if c == nil || c.State != "complete" || c.RepositoryID != target.RepositoryID || !strings.EqualFold(c.Repository, target.Repository) || c.PullNumber != target.PullNumber || c.HeadSHA != target.HeadSHA || c.BaseSHA != target.BaseSHA || c.BaseRef != target.BaseRef || c.MergeState != o.MergeState {
		return false
	}
	for _, check := range o.Assessment.Checks {
		if check.State == "failed" {
			return true
		}
	}
	return false
}
