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

func sourceObligationCount(ctx context.Context, tx *sql.Tx, project string, pull int64) (int64, error) {
	facts, err := adoptionholds.ReadFacts(ctx, tx, project, pull)
	return facts.Open, sourceObligationReadError(err)
}

func sourceObligationReadError(err error) error {
	var failure *core.Error
	if errors.As(err, &failure) && failure.Code == "repository_changed" {
		return sourceHold("repository_changed")
	}
	return err
}
