package reviews

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

// Versions 10 and 11 were assigned independently by the released main/review
// slices and the integrated development lineage. Recognize their exact physical
// prerequisites before interpreting pending version numbers. Never rewind the
// stamp or rewrite retained review bodies; ordinary migrations converge at 12.
func reconcileReviewSchemaLineage(ctx context.Context, conn *sql.Conn, version int) error {
	if version < 10 {
		return nil
	}
	incompatible := func() error {
		return core.Fail("schema_incompatible", "The retained reviews schema does not match a supported migration lineage; no migration was applied.", 500)
	}
	definition := func(name, kind string) (string, error) {
		var actualKind, body string
		err := conn.QueryRowContext(ctx, `SELECT type,sql FROM sqlite_schema WHERE name=?`, name).Scan(&actualKind, &body)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if actualKind != kind {
			return "", incompatible()
		}
		return strings.TrimSuffix(strings.TrimSpace(body), ";"), nil
	}
	canonical := func(body string) string { return strings.TrimSuffix(strings.TrimSpace(body), ";") }
	pull, err := definition("review_pull_versions", "table")
	if err != nil {
		return err
	}
	view, err := definition("review_pulls", "view")
	if err != nil {
		return err
	}
	oldPull := pull == canonical(HistorySchemaSQLV10()[0]) && view == canonical(HistoryReadViewSQLV10()["review_pulls"])
	selectedPull := pull == canonical(HistorySchemaSQL()[0]) && view == canonical(HistoryReadViewSQL()["review_pulls"])
	if !oldPull && !selectedPull {
		return incompatible()
	}
	table, err := definition("review_hosted_discovery_state", "table")
	if err != nil {
		return err
	}
	index, err := definition("review_run_versions_external_source", "index")
	if err != nil {
		return err
	}
	frontier := table == canonical(ExternalHostedSchemaSQL()[0]) && index == canonical(ExternalHostedSchemaSQL()[1])
	absentFrontier := table == "" && index == ""
	if !frontier && !absentFrontier {
		return incompatible()
	}
	var repair []string
	switch version {
	case 10:
		if !oldPull {
			return incompatible()
		}
		if absentFrontier {
			repair = ExternalHostedSchemaSQL()
		} // main v10: completed counters, no discovery.
	case 11:
		if !frontier {
			return incompatible()
		}
		if oldPull {
			repair = deliverySelectionMigrationSQL()
		} // review slice v11: discovery, no Delivery.
	case 12:
		if !frontier || !selectedPull {
			return incompatible()
		}
	default:
		return incompatible()
	}
	for _, statement := range repair {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}
