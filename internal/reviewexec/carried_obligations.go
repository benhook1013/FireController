package reviewexec

import (
	"context"
	"database/sql"
	"errors"

	"github.com/benhook1013/FireController/internal/adoptionholds"
	"github.com/benhook1013/FireController/internal/core"
)

// Carried obligations restrict execution without supplying native evidence or
// changing comparison coverage. Imported bodies never enter compact outcomes.
func requireSourceObligationsClear(ctx context.Context, tx *sql.Tx, project string, pull int64) error {
	pending, err := adoptionholds.HasOpen(ctx, tx, project, pull)
	if err != nil {
		return sourceObligationReadError(err)
	}
	if pending {
		return sourceHold("source_obligations_pending")
	}
	return nil
}

// The automatic dependency count is distinct from the admission guard above.
func sourceObligationCount(ctx context.Context, tx adoptionholds.Query, project string, pull int64) (int64, error) {
	facts, err := adoptionholds.ReadFacts(ctx, tx, project, pull)
	refusal := sourceObligationReadError(err)
	var failure *core.Error
	if errors.As(err, &failure) && failure.Code == "repository_changed" {
		// A retained binding mismatch prevents planning only for this project.
		// Keep the public source hold and do not enter unused-attempt recovery.
		return 0, &automaticProjectStateError{refusal}
	}
	return facts.Open, refusal
}

// Preserve the existing unused-attempt recovery for typed carried-obligation
// storage faults without classifying other project-local history as scheduling.
type sourceObligationProjectStateError struct{ error }

func (e *sourceObligationProjectStateError) Unwrap() error { return e.error }

func isSourceObligationProjectStateError(err error) bool {
	var source *sourceObligationProjectStateError
	return errors.As(err, &source)
}

func sourceObligationReadError(err error) error {
	var failure *core.Error
	if errors.As(err, &failure) && failure.Code == "repository_changed" {
		return sourceHold("repository_changed")
	}
	if adoptionholds.IsProjectStateError(err) {
		return &sourceObligationProjectStateError{&automaticProjectStateError{err}}
	}
	return err
}
