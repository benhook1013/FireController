package reviewexec

import (
	"context"
	"database/sql"

	"github.com/benhook1013/FireController/internal/adoptionholds"
)

// Carried obligations restrict execution without supplying native evidence or
// changing comparison coverage. Imported bodies never enter compact outcomes.
func requireSourceObligationsClear(ctx context.Context, tx *sql.Tx, project string, pull int64) error {
	pending, err := adoptionholds.HasOpen(ctx, tx, project, pull)
	if err != nil {
		return err
	}
	if pending {
		return sourceHold("source_obligations_pending")
	}
	return nil
}

func sourceObligationCount(ctx context.Context, tx *sql.Tx, project string, pull int64) (int64, error) {
	facts, err := adoptionholds.ReadFacts(ctx, tx, project, pull)
	return facts.Open, err
}
