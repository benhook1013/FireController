package reviewexec

import (
	"context"
	"database/sql"
	"errors"
)

type routePolicyFact struct {
	id, project      string
	target, revision int64
	active, pending  bool
}

type routePolicyWriter interface {
	EvidenceQuery
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func deriveRoutePolicyFact(ctx context.Context, q EvidenceQuery, project, id string) (routePolicyFact, error) {
	route, err := readFindingRoute(ctx, q, project, id)
	if err != nil {
		return routePolicyFact{}, err
	}
	fact := routePolicyFact{id: route.ID, project: route.ProjectID, target: route.TargetPullNumber, revision: route.Revision, active: route.State == "active"}
	fact.pending = fact.active && (route.Disposition == "unadjudicated" || route.Disposition == "accepted" && route.FixSHA == "")
	return fact, nil
}

// Persist only after current and historical route authority are both installed.
// Clearing one route never clears a different route's invalid receiving scope.
func refreshRoutePolicyFact(ctx context.Context, q routePolicyWriter, project, id string) error {
	fact, err := deriveRoutePolicyFact(ctx, q, project, id)
	if err != nil {
		return err
	}
	if _, err = q.ExecContext(ctx, `INSERT INTO review_route_policy_facts(route_id,project_id,target_pull_number,revision,active,pending) VALUES(?,?,?,?,?,?) ON CONFLICT(route_id) DO UPDATE SET project_id=excluded.project_id,target_pull_number=excluded.target_pull_number,revision=excluded.revision,active=excluded.active,pending=excluded.pending`, fact.id, fact.project, fact.target, fact.revision, fact.active, fact.pending); err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `DELETE FROM review_route_policy_invalidations WHERE route_id=?`, id)
	return err
}

func readRoutePolicyFact(ctx context.Context, q EvidenceQuery, id string) (routePolicyFact, error) {
	var fact routePolicyFact
	err := q.QueryRowContext(ctx, `SELECT route_id,project_id,target_pull_number,revision,active,pending FROM review_route_policy_facts WHERE route_id=?`, id).Scan(&fact.id, &fact.project, &fact.target, &fact.revision, &fact.active, &fact.pending)
	return fact, err
}

// ValidateRoutePolicyFacts is full read-only maintenance validation. Ordinary
// policy reads use the scoped invalidation and pending indexes instead.
func ValidateRoutePolicyFacts(ctx context.Context, q EvidenceQuery) error {
	var invalid bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_route_policy_invalidations) OR EXISTS(SELECT 1 FROM review_route_policy_facts p LEFT JOIN review_finding_routes r ON r.id=p.route_id WHERE p.route_id IS NULL OR p.route_id='' OR r.id IS NULL OR p.project_id!=r.project_id OR p.target_pull_number!=r.target_pull_number OR p.revision!=r.revision) OR EXISTS(SELECT 1 FROM review_finding_routes WHERE id IS NULL OR id='')`).Scan(&invalid); err != nil {
		return err
	}
	if invalid {
		return routeStorageError()
	}
	last := ""
	for {
		rows, err := q.QueryContext(ctx, `SELECT id,project_id FROM review_finding_routes WHERE id>? ORDER BY id LIMIT 128`, last)
		if err != nil {
			return err
		}
		type selected struct{ id, project string }
		var page []selected
		for rows.Next() {
			var row selected
			if err = rows.Scan(&row.id, &row.project); err != nil {
				break
			}
			page = append(page, row)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, row := range page {
			want, err := deriveRoutePolicyFact(ctx, q, row.project, row.id)
			if err != nil {
				return err
			}
			got, err := readRoutePolicyFact(ctx, q, row.id)
			if errors.Is(err, sql.ErrNoRows) || err == nil && got != want {
				return routeStorageError()
			}
			if err != nil {
				return err
			}
			last = row.id
		}
		if len(page) < 128 {
			return nil
		}
	}
}
