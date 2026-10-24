package reviews

import (
	"context"
	"database/sql"

	"github.com/benhook1013/FireController/internal/core"
)

// SourceStatus is a trusted, comparison-bound provider observation, not a
// readiness verdict or permission to request a review. Unknown is explicit.
type SourceStatus struct {
	Pull PullInput
	// Empty CIState means this poll did not collect CI. The publisher retains
	// it only for an unchanged comparison; explicit unknown clears old credit.
	CIState      string
	Mergeability string
}

// PublishSourceStatus shares source metadata's pre-I/O CAS, evidence retention
// and public publication boundary. The ordinary metadata API still refuses
// caller-supplied CI and mergeability; only this internal path can replace them.
func PublishSourceStatus(ctx context.Context, tx *sql.Tx, scope core.Scope, repositoryID int64, pins map[int64]SourceMetadataPin, observations []SourceStatus) (Receipt, error) {
	if len(observations) > maxQueuePulls {
		return Receipt{}, validationError("Select bounded source observations.")
	}
	pulls := make([]PullInput, 0, len(observations))
	statuses := make(map[int64]SourceStatus, len(observations))
	for _, observation := range observations {
		if !oneOf(observation.CIState, "", "unknown", "pending", "success", "failure", "not_required") || !oneOf(observation.Mergeability, "unknown", "mergeable", "conflicting") {
			return Receipt{}, validationError("Source status contains an unsupported observation.")
		}
		pulls = append(pulls, observation.Pull)
		statuses[observation.Pull.Number] = observation
	}
	return publishSourceMetadata(ctx, tx, scope, repositoryID, pins, pulls, statuses)
}
