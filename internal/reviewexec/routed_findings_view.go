package reviewexec

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/adoptionholds"
	m "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/core"
)

type RoutedFindingCounts struct {
	Open     int64 `json:"open"`
	Fixed    int64 `json:"fixed"`
	Rejected int64 `json:"rejected"`
	Resolved int64 `json:"resolved"`
}

type RoutedFindingItem struct {
	ID                string  `json:"id"`
	Kind              string  `json:"kind"`
	State             string  `json:"state"`
	SourcePullNumbers []int64 `json:"source_pull_numbers"`
	TargetPullNumber  int64   `json:"target_pull_number,omitempty"`
	TargetState       string  `json:"target_state"`
	Title             string  `json:"title"`
	Revision          int64   `json:"revision"`
	CreatedAt         string  `json:"created_at"`
	SourceEvidenceID  string  `json:"source_evidence_id,omitempty"`
}

type RoutedFindingsView struct {
	ProjectID  string              `json:"project_id"`
	View       string              `json:"view"`
	PullNumber int64               `json:"pull_number"`
	Counts     RoutedFindingCounts `json:"counts"`
	Items      []RoutedFindingItem `json:"items"`
	Cursor     string              `json:"cursor,omitempty"`
	HasMore    bool                `json:"has_more"`
}

// The target pointer is an explicit retained reference, not text interpreted as
// a command or evidence of a source PR. Missing/ambiguous pointers stay unknown.
func routedCarriedTarget(evidence []string) int64 {
	return m.OriginalRouteTarget(m.Obligation{Kind: "route", Evidence: evidence})
}

func routedFindingTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			continue
		}
		if len(line) <= 512 {
			return line
		}
		end := 509
		for !utf8.RuneStart(line[end]) {
			end--
		}
		return line[:end] + "…"
	}
	return "Routed finding"
}

// Counts and page selection share one read snapshot. Only selected page entries
// load finding text/provenance; history inventories are neither copied nor drained.
func (e *Engine) ReadRoutedFindings(ctx context.Context, scope core.Scope, pull int64, limit int, cursor string) (RoutedFindingsView, error) {
	if ctx == nil || e == nil || e.store == nil || pull != 0 && !validRoutePull(pull) || limit < 0 || limit > 100 {
		return RoutedFindingsView{}, invalid()
	}
	if limit == 0 {
		limit = 20
	}
	view := "attention"
	if pull != 0 {
		view = "pull"
	}
	filter := "reviews.routed-findings:v1:" + strconv.FormatInt(pull, 10)
	after, err := e.store.ParseCursor(scope, filter, cursor)
	if err != nil {
		return RoutedFindingsView{}, err
	}
	afterKind, afterID := "", ""
	if after != "" {
		var found bool
		afterKind, afterID, found = strings.Cut(after, "|")
		if !found || afterKind != "native" && afterKind != "carried" || !identifier.MatchString(afterID) {
			return RoutedFindingsView{}, invalid()
		}
	}
	out := RoutedFindingsView{ProjectID: scope.ProjectID, View: view, PullNumber: pull, Items: []RoutedFindingItem{}}
	err = e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		// Execution can have retained routes before any browser PR metadata exists.
		var history bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='review_current_pulls')`).Scan(&history); err != nil {
			return err
		}
		query := routedFindingsQuery(history)
		args := []any{scope.ProjectID, pull}
		var invalidCount int64
		if err := tx.QueryRowContext(ctx, query+` SELECT coalesce(sum(state='open'),0),coalesce(sum(state='fixed'),0),coalesce(sum(state='rejected'),0),coalesce(sum(state='resolved'),0),coalesce(sum(state='invalid'),0) FROM selected`, args...).Scan(&out.Counts.Open, &out.Counts.Fixed, &out.Counts.Rejected, &out.Counts.Resolved, &invalidCount); err != nil {
			return err
		}
		if invalidCount != 0 || out.Counts.Open < 0 || out.Counts.Fixed < 0 || out.Counts.Rejected < 0 || out.Counts.Resolved < 0 || out.Counts.Open > 1<<53-1 || out.Counts.Fixed > 1<<53-1 || out.Counts.Rejected > 1<<53-1 || out.Counts.Resolved > 1<<53-1 {
			return routeStorageError()
		}
		pageArgs := append(append([]any{}, args...), afterKind, afterID, limit+1)
		rows, err := tx.QueryContext(ctx, query+` SELECT id,kind,state,target,target_state,revision,created_at FROM selected WHERE (kind,id)>(?,?) ORDER BY kind,id LIMIT ?`, pageArgs...)
		if err != nil {
			return err
		}
		var page []RoutedFindingItem
		for rows.Next() {
			var item RoutedFindingItem
			if err = rows.Scan(&item.ID, &item.Kind, &item.State, &item.TargetPullNumber, &item.TargetState, &item.Revision, &item.CreatedAt); err != nil {
				break
			}
			page = append(page, item)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		out.HasMore = len(page) > limit
		if out.HasMore {
			page = page[:limit]
		}
		for _, item := range page {
			item.SourcePullNumbers = []int64{}
			if item.Kind == "native" {
				route, err := readFindingRoute(ctx, tx, scope.ProjectID, item.ID)
				if err != nil {
					return err
				}
				evidence, state, err := sourceRouteState(ctx, tx, scope.ProjectID, route.SourceEvidenceID)
				if err != nil {
					return err
				}
				if !routeMatchesSource(route, evidence, state, true) || route.TargetPullNumber != item.TargetPullNumber || route.Revision != item.Revision || route.CreatedAt != item.CreatedAt {
					return routeStorageError()
				}
				item.SourcePullNumbers = []int64{route.SourcePullNumber}
				item.SourceEvidenceID = route.SourceEvidenceID
				item.Title = routedFindingTitle(evidence.Capture.Review.Findings[route.Finding].Title)
			} else {
				detail, err := adoptionholds.ReadDetail(ctx, tx, scope.ProjectID, item.ID)
				if err != nil {
					return err
				}
				currentTarget := routedCarriedTarget(detail.Original.Evidence)
				if detail.Retarget != nil {
					currentTarget = detail.Retarget.TargetPullNumber
				}
				if detail.Original.Kind != "route" || detail.Status != item.State || detail.Revision != item.Revision || detail.CreatedAt != item.CreatedAt || currentTarget != item.TargetPullNumber {
					return routeStorageError()
				}
				item.Title = routedFindingTitle(detail.Original.Summary)
			}
			if item.TargetPullNumber < 0 {
				item.TargetPullNumber = 0
			}
			out.Items = append(out.Items, item)
		}
		if out.HasMore {
			last := out.Items[len(out.Items)-1]
			out.Cursor = e.store.Cursor(scope, filter, last.Kind+"|"+last.ID)
		}
		return nil
	})
	if err != nil {
		return RoutedFindingsView{}, err
	}
	return out, nil
}

func routedFindingsQuery(history bool) string {
	pulls := `SELECT 0 AS number,'' AS state WHERE 0`
	if history {
		// Current retained membership, including merged history outside Delivery's
		// bounded queue, owns lifecycle. Missing membership never means closed.
		pulls = `SELECT c.number,v.state FROM review_current_pulls c JOIN review_pull_memberships m ON m.project_id=c.project_id AND m.number=c.number AND m.start_revision=c.start_revision AND m.position=c.position AND m.end_revision IS NULL JOIN review_pull_versions v ON v.project_id=m.project_id AND v.number=m.number AND v.version_revision=m.version_revision WHERE c.project_id=(SELECT project FROM scope)`
	}
	return `WITH scope AS (SELECT ? AS project,? AS pull), pulls AS (` + pulls + `),
	carried AS (SELECT o.id,o.status,o.revision,o.created_at,(SELECT CASE WHEN count(*)=1 THEN min(value) ELSE '' END FROM json_each(o.original_json,'$.evidence') WHERE type='text' AND substr(value,1,10)='target-pr:') AS marker,(SELECT r.target_pull_number FROM review_adoption_retargets r WHERE r.obligation_id=o.id ORDER BY r.revision DESC LIMIT 1) AS current_target FROM review_adoption_obligations o WHERE o.project_id=(SELECT project FROM scope) AND json_extract(o.original_json,'$.kind')='route'),
	carried_targets AS (SELECT *,CASE WHEN current_target IS NOT NULL THEN current_target WHEN marker='target-pr:unassigned' THEN 0 WHEN substr(marker,11)=CAST(CAST(substr(marker,11) AS INTEGER) AS TEXT) AND CAST(substr(marker,11) AS INTEGER) BETWEEN 1 AND 2147483647 THEN CAST(substr(marker,11) AS INTEGER) ELSE -1 END AS target FROM carried),
	all_routes AS (
	SELECT r.id,'native' AS kind,CASE WHEN r.disposition='unadjudicated' AND r.has_fix=0 OR r.disposition='accepted' AND r.has_fix=0 THEN 'open' WHEN r.disposition='accepted' AND r.has_fix=1 THEN 'fixed' WHEN r.disposition='rejected' AND r.has_fix=0 THEN 'rejected' ELSE 'invalid' END AS state,r.target_pull_number AS target,r.revision,r.created_at FROM review_finding_routes r WHERE r.project_id=(SELECT project FROM scope) AND r.state='active' AND ((SELECT pull FROM scope)=0 OR r.source_pull_number=(SELECT pull FROM scope) OR r.target_pull_number=(SELECT pull FROM scope))
	UNION ALL SELECT c.id,'carried',CASE WHEN c.status IN ('open','resolved') THEN c.status ELSE 'invalid' END,c.target,c.revision,c.created_at FROM carried_targets c WHERE (SELECT pull FROM scope)=0 OR c.target=(SELECT pull FROM scope) OR EXISTS(SELECT 1 FROM review_adoption_targets t WHERE t.project_id=(SELECT project FROM scope) AND t.obligation_id=c.id AND t.pull_number=(SELECT pull FROM scope))),
	with_states AS (SELECT a.*,CASE WHEN target=0 THEN 'unassigned' WHEN p.state IN ('open','closed','merged') THEN p.state ELSE 'unknown' END AS target_state FROM all_routes a LEFT JOIN pulls p ON p.number=a.target),
	selected AS (SELECT * FROM with_states WHERE ((SELECT pull FROM scope)=0 AND state IN ('open','invalid') AND target_state IN ('unassigned','closed','merged','unknown')) OR (SELECT pull FROM scope)<>0)`
}
