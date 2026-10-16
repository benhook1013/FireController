package reviewexec

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

// WithNativeProjection connects journal transitions and adjudication changes
// to the native browser projection in the same transaction. The owner must
// install the reviews and evidence schemas before accepting requests. Isolated journal users
// need not install a browser projection.
func WithNativeProjection() EngineOption {
	return func(e *Engine) {
		e.publishAdjudication = publishCLIAdjudication
		e.publishAttempt = publishAttemptTransition
	}
}

func cliProjectionFindingID(evidenceID string, finding int64) string {
	return evidenceID + ":finding:" + strconv.FormatInt(finding, 10)
}

func publishCLIAdjudication(ctx context.Context, tx *sql.Tx, scope core.Scope, state CLIAdjudication) error {
	input := reviews.NativeAdjudication{
		SourceRunID: state.EvidenceID,
		Finalized:   state.Finalized,
		Findings:    make([]reviews.NativeFindingUpdate, 0, len(state.Findings)),
	}
	if state.FrozenCounts != nil {
		input.FrozenCounts = &reviews.NativeCounts{
			Found:    state.FrozenCounts.Found,
			Accepted: state.FrozenCounts.Accepted,
			Routed:   state.FrozenCounts.Routed,
		}
	}
	for _, finding := range state.Findings {
		input.Findings = append(input.Findings, reviews.NativeFindingUpdate{
			SourceFindingID: cliProjectionFindingID(state.EvidenceID, finding.Finding),
			Disposition:     finding.Disposition,
			DecisionReason:  finding.Reason,
			Actor:           finding.ActorID,
			DecidedAt:       finding.DecidedAt,
			FixSHA:          finding.FixSHA,
			ProofNote:       finding.ProofNote,
			ResolvedAt:      finding.FixedAt,
		})
	}
	_, _, err := reviews.UpdateNativeAdjudication(ctx, tx, scope, input)
	return err
}
