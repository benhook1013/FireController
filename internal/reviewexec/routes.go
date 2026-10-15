package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type FindingRouteRequest struct {
	ExpectedRevision      *int64 `json:"expected_revision"`
	Finding               int64  `json:"finding"`
	TargetPullNumber      int64  `json:"target_pull_number"`
	Reason                string `json:"reason"`
	ExpectedRouteRevision int64  `json:"expected_route_revision,omitempty"`
}
type FindingRouteDecisionRequest struct {
	ExpectedRevision *int64 `json:"expected_revision"`
	Disposition      string `json:"disposition"`
	Reason           string `json:"reason"`
}
type FindingRouteFixRequest struct {
	ExpectedRevision *int64  `json:"expected_revision"`
	ExpectedSHA      *string `json:"expected_sha"`
	FixSHA           string  `json:"fix_sha"`
	ProofNote        string  `json:"proof_note"`
}
type ReviewRoute struct {
	ID               string `json:"id"`
	ProjectID        string `json:"project_id"`
	RepositoryID     int64  `json:"repository_id"`
	SourceEvidenceID string `json:"source_evidence_id"`
	Finding          int64  `json:"finding"`
	SourcePullNumber int64  `json:"source_pull_number"`
	SourceHeadSHA    string `json:"source_head_sha"`
	TargetPullNumber int64  `json:"target_pull_number"`
	State            string `json:"state"`
	Revision         int64  `json:"revision"`
	Disposition      string `json:"disposition"`
	RoutingReason    string `json:"routing_reason"`
	RoutingActorID   string `json:"routing_actor_id"`
	Reason           string `json:"reason"`
	ActorID          string `json:"actor_id"`
	DecidedAt        string `json:"decided_at"`
	FixSHA           string `json:"fix_sha"`
	ProofNote        string `json:"proof_note"`
	FixedAt          string `json:"fixed_at"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}
type FindingRouteDetail struct {
	Route         ReviewRoute                  `json:"route"`
	SourceAnchor  Anchor                       `json:"source_anchor"`
	SourceFinding reviewprovider.ParsedFinding `json:"source_finding"`
}
type FindingRouteSummary struct {
	ID               string `json:"id"`
	Revision         int64  `json:"revision"`
	SourceEvidenceID string `json:"source_evidence_id"`
	Finding          int64  `json:"finding"`
	SourcePullNumber int64  `json:"source_pull_number"`
	TargetPullNumber int64  `json:"target_pull_number"`
	Disposition      string `json:"disposition"`
	HasFix           bool   `json:"has_fix"`
	CreatedAt        string `json:"created_at"`
}
type FindingRoutesPage struct {
	ProjectID  string                `json:"project_id"`
	PullNumber int64                 `json:"pull_number"`
	Items      []FindingRouteSummary `json:"items"`
	Cursor     string                `json:"cursor,omitempty"`
	HasMore    bool                  `json:"has_more"`
}
type findingRouteReceipt struct {
	ID             string `json:"id"`
	State          string `json:"state"`
	Revision       int64  `json:"revision"`
	SourceRevision int64  `json:"source_revision"`
}

// routeStamp samples only local mutation recording time. Route timestamps use
// one fixed-width UTC representation, so their lexical order is chronological.
func (e *Engine) routeStamp(minimum string) string {
	now := time.Now
	if e.routeNow != nil {
		now = e.routeNow
	}
	return routeStampAtLeast(now().UTC().Format("2006-01-02T15:04:05.000000000Z"), minimum)
}
func routeStampAtLeast(at, minimum string) string {
	if at < minimum {
		return minimum
	}
	return at
}

func validFindingRouteID(id string) bool { return hostedObservationID(id, "review_route_") }
func validRoutePull(n int64) bool        { return n > 0 && n <= 2147483647 }
func routeStorageError() error {
	return core.Fail("storage_unavailable", "Stored finding route is unavailable.", 500)
}
func routeTimestamp(s string) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339Nano, s)
	return at, err == nil && !at.IsZero() && s == at.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
func validReviewRoute(r ReviewRoute) bool {
	created, createdOK := routeTimestamp(r.CreatedAt)
	updated, updatedOK := routeTimestamp(r.UpdatedAt)
	if !validFindingRouteID(r.ID) || !identifier.MatchString(r.ProjectID) || r.RepositoryID <= 0 || !hostedObservationID(r.SourceEvidenceID, "review_evidence_") || r.Finding < 0 || r.Finding >= 100 || !validRoutePull(r.SourcePullNumber) || !validRoutePull(r.TargetPullNumber) || r.SourcePullNumber == r.TargetPullNumber || !sha.MatchString(r.SourceHeadSHA) || r.Revision < 1 || !safeText(r.RoutingReason, 2048) || strings.TrimSpace(r.RoutingReason) == "" || !identifier.MatchString(r.RoutingActorID) || !createdOK || !updatedOK || updated.Before(created) || !safeText(r.Reason, 2048) || !safeText(r.ProofNote, 2048) {
		return false
	}
	if r.State != "active" && r.State != "superseded" {
		return false
	}
	switch r.Disposition {
	case "unadjudicated":
		if r.Reason != "" || r.ActorID != "" || r.DecidedAt != "" || r.FixSHA != "" {
			return false
		}
	case "accepted", "rejected":
		decided, ok := routeTimestamp(r.DecidedAt)
		if !ok || decided.Before(created) || decided.After(updated) || !identifier.MatchString(r.ActorID) || strings.TrimSpace(r.Reason) == "" {
			return false
		}
	default:
		return false
	}
	if r.FixSHA != "" {
		fixed, ok := routeTimestamp(r.FixedAt)
		if r.Disposition != "accepted" || r.State != "active" || !sha.MatchString(r.FixSHA) || strings.TrimSpace(r.ProofNote) == "" || !ok || fixed.Before(created) || fixed.After(updated) {
			return false
		}
	} else if r.ProofNote != "" || r.FixedAt != "" {
		return false
	}
	return true
}
func decodeReviewRoute(body string) (ReviewRoute, error) {
	var route ReviewRoute
	if len(body) > 64<<10 || strictjson.Decode([]byte(body), &route) != nil || !validReviewRoute(route) || encoded(route) != body {
		return route, routeStorageError()
	}
	return route, nil
}
func sameRouteOrigin(a, b ReviewRoute) bool {
	return a.ID == b.ID && a.ProjectID == b.ProjectID && a.RepositoryID == b.RepositoryID && a.SourceEvidenceID == b.SourceEvidenceID && a.Finding == b.Finding && a.SourcePullNumber == b.SourcePullNumber && a.SourceHeadSHA == b.SourceHeadSHA && a.TargetPullNumber == b.TargetPullNumber && a.RoutingReason == b.RoutingReason && a.RoutingActorID == b.RoutingActorID && a.CreatedAt == b.CreatedAt
}

// readFindingRoute never loads source adjudication, avoiding recursive loaders.
// Consumers validate the current source link before using an active obligation.
func readFindingRoute(ctx context.Context, tx *sql.Tx, project, id string) (ReviewRoute, error) {
	var route ReviewRoute
	var repositoryID, finding, sourcePull, targetPull, revision, hasFix int64
	var sourceEvidence, head, state, disposition, created, body string
	var currentHistory, original sql.NullString
	var currentActor, currentKind, currentAt, originActor, originKind, originAt sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT r.repository_id,r.source_evidence_id,r.finding_index,r.source_pull_number,r.source_head_sha,r.target_pull_number,r.state,r.revision,r.disposition,r.has_fix,r.created_at,r.body_json,h.body_json,o.body_json,h.actor_id,h.kind,h.created_at,o.actor_id,o.kind,o.created_at FROM review_finding_routes r LEFT JOIN review_finding_route_revisions h ON h.route_id=r.id AND h.project_id=r.project_id AND h.revision=r.revision LEFT JOIN review_finding_route_revisions o ON o.route_id=r.id AND o.project_id=r.project_id AND o.revision=1 WHERE r.project_id=? AND r.id=?`, project, id).Scan(&repositoryID, &sourceEvidence, &finding, &sourcePull, &head, &targetPull, &state, &revision, &disposition, &hasFix, &created, &body, &currentHistory, &original, &currentActor, &currentKind, &currentAt, &originActor, &originKind, &originAt)
	if errors.Is(err, sql.ErrNoRows) {
		return route, core.Fail("not_found", "The finding route is unavailable in this project.", 404)
	}
	if err != nil {
		return route, err
	}
	if !currentHistory.Valid || !original.Valid || !currentActor.Valid || !currentKind.Valid || !currentAt.Valid || !originActor.Valid || !originKind.Valid || !originAt.Valid {
		return route, routeStorageError()
	}
	route, err = decodeReviewRoute(body)
	if err != nil {
		return route, err
	}
	origin, err := decodeReviewRoute(original.String)
	if err != nil {
		return route, err
	}
	if route.ID != id || route.ProjectID != project || !sameRouteOrigin(route, origin) || origin.Revision != 1 || origin.State != "active" || origin.Disposition != "unadjudicated" || route.RepositoryID != repositoryID || route.SourceEvidenceID != sourceEvidence || route.Finding != finding || route.SourcePullNumber != sourcePull || route.SourceHeadSHA != head || route.TargetPullNumber != targetPull || route.State != state || route.Revision != revision || route.Disposition != disposition || route.CreatedAt != created || hasFix != 0 && hasFix != 1 || (route.FixSHA != "") != (hasFix == 1) || currentHistory.String != body || !identifier.MatchString(currentActor.String) || currentAt.String != route.UpdatedAt || originActor.String != route.RoutingActorID || originKind.String != "routed" || originAt.String != route.CreatedAt || origin.UpdatedAt != origin.CreatedAt {
		return route, routeStorageError()
	}
	switch currentKind.String {
	case "routed", "superseded", "adjudicated", "fix_recorded":
	default:
		return route, routeStorageError()
	}
	return route, nil
}

// sourceRouteState validates the retained source against its indexed journal
// identity; it does not consult providers or current executor availability.
func sourceRouteState(ctx context.Context, tx *sql.Tx, project, id string) (ReviewEvidence, ReviewAdjudication, error) {
	evidence, state, err := loadReviewAdjudication(ctx, tx, project, id)
	if err != nil {
		return evidence, state, err
	}
	var attemptID, operationID string
	if err = tx.QueryRowContext(ctx, `SELECT attempt_id,operation_id FROM review_execution_evidence WHERE project_id=? AND id=?`, project, id).Scan(&attemptID, &operationID); err != nil {
		return evidence, state, err
	}
	if evidence.AttemptID != attemptID || evidence.OperationID != operationID || !hostedObservationID(attemptID, "review_attempt_") || !hostedObservationID(operationID, "review_operation_") || !validEvidenceAnchor(evidence.Anchor) || evidence.Anchor != evidence.Capture.ExpectedAnchor || !hash.MatchString(evidence.CapabilityFingerprint) {
		return evidence, state, routeStorageError()
	}
	attempt, err := readAttempt(ctx, tx, project, attemptID)
	if err != nil {
		return evidence, state, err
	}
	operation, err := readOperation(ctx, tx, project, operationID)
	if err != nil {
		return evidence, state, err
	}
	if attempt.Anchor == nil || *attempt.Anchor != evidence.Anchor || attempt.OperationID != operationID || operation.AttemptID != attemptID || operation.HeadSHA != evidence.Anchor.CandidateSHA || operation.PullNumber != evidence.Anchor.PullNumber || operation.CapabilityFingerprint != evidence.CapabilityFingerprint || operation.Channel != evidenceChannel(evidence.Capture) || operation.Channel == "hosted" && validateHostedEvidenceIdentity(attempt, operation, evidence.Capture) != nil {
		return evidence, state, routeStorageError()
	}
	return evidence, state, nil
}
func routeMatchesSource(route ReviewRoute, evidence ReviewEvidence, state ReviewAdjudication, active bool) bool {
	if route.ProjectID != evidence.ProjectID || route.RepositoryID != evidence.Anchor.RepositoryID || route.SourceEvidenceID != evidence.ID || route.SourcePullNumber != evidence.Anchor.PullNumber || route.SourceHeadSHA != evidence.Anchor.CandidateSHA || route.Finding < 0 || route.Finding >= int64(len(state.Findings)) {
		return false
	}
	if active {
		f := state.Findings[route.Finding]
		return route.State == "active" && f.Disposition == "routed" && f.RouteID == route.ID && f.Reason == route.RoutingReason && f.ActorID == route.RoutingActorID && f.DecidedAt == route.CreatedAt
	}
	return true
}
func routeRepository(ctx context.Context, tx *sql.Tx, project string, repositoryID int64) error {
	var current int64
	if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&current); err != nil {
		return err
	}
	if current != repositoryID {
		return sourceHold("repository_changed")
	}
	return nil
}
func persistFindingRoute(ctx context.Context, tx *sql.Tx, scope core.Scope, route ReviewRoute, prior int64, kind string) error {
	if !validReviewRoute(route) {
		return routeStorageError()
	}
	var result sql.Result
	var err error
	if prior == 0 {
		result, err = tx.ExecContext(ctx, `INSERT INTO review_finding_routes(id,project_id,repository_id,source_evidence_id,finding_index,source_pull_number,source_head_sha,target_pull_number,state,revision,disposition,has_fix,created_at,body_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, route.ID, route.ProjectID, route.RepositoryID, route.SourceEvidenceID, route.Finding, route.SourcePullNumber, route.SourceHeadSHA, route.TargetPullNumber, route.State, route.Revision, route.Disposition, route.FixSHA != "", route.CreatedAt, encoded(route))
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE review_finding_routes SET state=?,revision=?,disposition=?,has_fix=?,body_json=? WHERE project_id=? AND id=? AND revision=?`, route.State, route.Revision, route.Disposition, route.FixSHA != "", encoded(route), scope.ProjectID, route.ID, prior)
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return conflict()
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO review_finding_route_revisions(route_id,project_id,revision,actor_id,kind,created_at,body_json) VALUES(?,?,?,?,?,?,?)`, route.ID, route.ProjectID, route.Revision, scope.PrincipalID, kind, route.UpdatedAt, encoded(route)); err != nil {
		return err
	}
	return core.AppendEvent(ctx, tx, scope, "review.route."+kind, route.ID, route.Revision, struct {
		EvidenceID       string `json:"evidence_id"`
		Finding          int64  `json:"finding"`
		TargetPullNumber int64  `json:"target_pull_number"`
	}{route.SourceEvidenceID, route.Finding, route.TargetPullNumber})
}
func supersedeFindingRoute(ctx context.Context, tx *sql.Tx, scope core.Scope, evidence ReviewEvidence, state ReviewAdjudication, finding FindingDecision, expected int64, at string) (string, error) {
	route, err := readFindingRoute(ctx, tx, scope.ProjectID, finding.RouteID)
	if err != nil {
		return "", err
	}
	if !routeMatchesSource(route, evidence, state, true) {
		return "", routeStorageError()
	}
	if route.Revision != expected || route.Revision == math.MaxInt64 || route.FixSHA != "" {
		return "", conflict()
	}
	prior := route.Revision
	route.Revision++
	route.State = "superseded"
	route.UpdatedAt = routeStampAtLeast(at, route.UpdatedAt)
	if err = persistFindingRoute(ctx, tx, scope, route, prior, "superseded"); err != nil {
		return "", err
	}
	return route.UpdatedAt, nil
}

func (e *Engine) CreateFindingRoute(ctx context.Context, scope core.Scope, key, evidenceID string, request FindingRouteRequest) (json.RawMessage, error) {
	if ctx == nil || e == nil || e.store == nil || !hostedObservationID(evidenceID, "review_evidence_") || !validAdjudicationRevision(request.ExpectedRevision) || request.Finding < 0 || request.Finding >= 100 || !validRoutePull(request.TargetPullNumber) || !safeText(request.Reason, 2048) || strings.TrimSpace(request.Reason) == "" || request.ExpectedRouteRevision < 0 || request.ExpectedRouteRevision == math.MaxInt64 {
		return nil, invalid()
	}
	expected := *request.ExpectedRevision
	request.ExpectedRevision = &expected
	return e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.evidence.route", Target: evidenceID, Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		evidence, state, err := sourceRouteState(ctx, tx, scope.ProjectID, evidenceID)
		if err != nil {
			return nil, err
		}
		if !adjudicationMatches(state, expected) {
			return nil, conflict()
		}
		if request.Finding >= int64(len(state.Findings)) {
			return nil, invalid()
		}
		if err = routeRepository(ctx, tx, scope.ProjectID, evidence.Anchor.RepositoryID); err != nil {
			return nil, err
		}
		c, _, err := readConfiguration(ctx, tx, scope.ProjectID)
		if err != nil {
			return nil, err
		}
		if validateConfigurationShape(c) != nil {
			return nil, routeStorageError()
		}
		if request.TargetPullNumber == evidence.Anchor.PullNumber || !slices.Contains(c.Queue, request.TargetPullNumber) {
			return nil, invalid()
		}
		f := &state.Findings[request.Finding]
		if f.FixSHA != "" {
			return nil, conflict()
		}
		at := e.routeStamp("")
		if f.Disposition == "routed" {
			if at, err = supersedeFindingRoute(ctx, tx, scope, evidence, state, *f, request.ExpectedRouteRevision, at); err != nil {
				return nil, err
			}
		} else if request.ExpectedRouteRevision != 0 {
			return nil, conflict()
		}
		route := ReviewRoute{ID: core.NewID("review_route"), ProjectID: scope.ProjectID, RepositoryID: evidence.Anchor.RepositoryID, SourceEvidenceID: evidenceID, Finding: request.Finding, SourcePullNumber: evidence.Anchor.PullNumber, SourceHeadSHA: evidence.Anchor.CandidateSHA, TargetPullNumber: request.TargetPullNumber, State: "active", Revision: 1, Disposition: "unadjudicated", RoutingReason: request.Reason, RoutingActorID: scope.PrincipalID, CreatedAt: at, UpdatedAt: at}
		if err = persistFindingRoute(ctx, tx, scope, route, 0, "routed"); err != nil {
			return nil, err
		}
		f.Disposition, f.Reason, f.ActorID, f.DecidedAt, f.RouteID = "routed", request.Reason, scope.PrincipalID, at, route.ID
		if _, err = e.persistReviewAdjudication(ctx, tx, scope, state, "routed", request.Finding, request, at); err != nil {
			return nil, err
		}
		return findingRouteReceipt{ID: route.ID, State: "routed", Revision: 1, SourceRevision: state.Revision + 1}, nil
	})
}

func (e *Engine) mutateFindingRoute(ctx context.Context, scope core.Scope, key, id, kind string, expected int64, request any, change func(*ReviewRoute, string) error) (json.RawMessage, error) {
	return e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.route." + kind, Target: id, Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		route, err := readFindingRoute(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return nil, err
		}
		if route.State != "active" || route.Revision != expected || route.Revision == math.MaxInt64 {
			return nil, conflict()
		}
		if err = routeRepository(ctx, tx, scope.ProjectID, route.RepositoryID); err != nil {
			return nil, err
		}
		evidence, state, err := sourceRouteState(ctx, tx, scope.ProjectID, route.SourceEvidenceID)
		if err != nil {
			return nil, err
		}
		if !routeMatchesSource(route, evidence, state, true) {
			return nil, routeStorageError()
		}
		at := e.routeStamp(route.UpdatedAt)
		if err = change(&route, at); err != nil {
			return nil, err
		}
		route.Revision++
		route.UpdatedAt = at
		if err = persistFindingRoute(ctx, tx, scope, route, expected, kind); err != nil {
			return nil, err
		}
		if e.publishAdjudication != nil {
			if err = e.publishAdjudication(ctx, tx, scope, state); err != nil {
				return nil, err
			}
		}
		return Receipt{ID: id, State: kind, Revision: route.Revision}, nil
	})
}
func (e *Engine) DecideFindingRoute(ctx context.Context, scope core.Scope, key, id string, request FindingRouteDecisionRequest) (json.RawMessage, error) {
	if ctx == nil || e == nil || e.store == nil || !validFindingRouteID(id) || request.ExpectedRevision == nil || *request.ExpectedRevision < 1 || *request.ExpectedRevision == math.MaxInt64 || request.Disposition != "accepted" && request.Disposition != "rejected" || !safeText(request.Reason, 2048) || strings.TrimSpace(request.Reason) == "" {
		return nil, invalid()
	}
	expected := *request.ExpectedRevision
	request.ExpectedRevision = &expected
	return e.mutateFindingRoute(ctx, scope, key, id, "adjudicated", expected, request, func(r *ReviewRoute, at string) error {
		if r.FixSHA != "" && request.Disposition == "rejected" {
			return conflict()
		}
		r.Disposition, r.Reason, r.ActorID, r.DecidedAt = request.Disposition, request.Reason, scope.PrincipalID, at
		return nil
	})
}
func (e *Engine) RecordFindingRouteFix(ctx context.Context, scope core.Scope, key, id string, request FindingRouteFixRequest) (json.RawMessage, error) {
	if ctx == nil || e == nil || e.store == nil || !validFindingRouteID(id) || request.ExpectedRevision == nil || *request.ExpectedRevision < 1 || *request.ExpectedRevision == math.MaxInt64 || request.ExpectedSHA == nil || *request.ExpectedSHA != "" && !sha.MatchString(*request.ExpectedSHA) || !sha.MatchString(request.FixSHA) || !safeText(request.ProofNote, 2048) || strings.TrimSpace(request.ProofNote) == "" {
		return nil, invalid()
	}
	expected, expectedSHA := *request.ExpectedRevision, *request.ExpectedSHA
	request.ExpectedRevision, request.ExpectedSHA = &expected, &expectedSHA
	return e.mutateFindingRoute(ctx, scope, key, id, "fix_recorded", expected, request, func(r *ReviewRoute, at string) error {
		if r.Disposition != "accepted" || r.FixSHA != expectedSHA {
			return conflict()
		}
		r.FixSHA, r.ProofNote, r.FixedAt = request.FixSHA, request.ProofNote, at
		return nil
	})
}
func (e *Engine) ReadFindingRoute(ctx context.Context, scope core.Scope, id string) (FindingRouteDetail, error) {
	if ctx == nil || e == nil || e.store == nil || !validFindingRouteID(id) {
		return FindingRouteDetail{}, invalid()
	}
	var result FindingRouteDetail
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		route, err := readFindingRoute(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return err
		}
		evidence, state, err := sourceRouteState(ctx, tx, scope.ProjectID, route.SourceEvidenceID)
		if err != nil {
			return err
		}
		if !routeMatchesSource(route, evidence, state, route.State == "active") {
			return routeStorageError()
		}
		result = FindingRouteDetail{Route: route, SourceAnchor: evidence.Anchor, SourceFinding: evidence.Capture.Review.Findings[route.Finding]}
		return nil
	})
	if err != nil {
		return FindingRouteDetail{}, err
	}
	return result, nil
}

func (e *Engine) ListFindingRoutes(ctx context.Context, scope core.Scope, targetPR int64, limit int, cursor string) (FindingRoutesPage, error) {
	if ctx == nil || e == nil || e.store == nil || !validRoutePull(targetPR) || limit < 0 || limit > 100 {
		return FindingRoutesPage{}, invalid()
	}
	if limit == 0 {
		limit = 5
	}
	filter := "reviews.routes:v1:" + strconv.FormatInt(targetPR, 10)
	position, err := e.store.ParseCursor(scope, filter, cursor)
	if err != nil {
		return FindingRoutesPage{}, err
	}
	at, id := "", ""
	if position != "" {
		at, id, _ = strings.Cut(position, "|")
		if _, ok := routeTimestamp(at); !ok || !validFindingRouteID(id) {
			return FindingRoutesPage{}, invalid()
		}
	}
	result := FindingRoutesPage{ProjectID: scope.ProjectID, PullNumber: targetPR, Items: []FindingRouteSummary{}}
	err = e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		query := `SELECT id FROM review_finding_routes WHERE project_id=? AND target_pull_number=? AND state='active'`
		args := []any{scope.ProjectID, targetPR}
		if position != "" {
			query += ` AND (created_at,id)<(?,?)`
			args = append(args, at, id)
		}
		query += ` ORDER BY created_at DESC,id DESC LIMIT ?`
		args = append(args, limit+1)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		for i, id := range ids {
			r, err := readFindingRoute(ctx, tx, scope.ProjectID, id)
			if err != nil {
				return err
			}
			if r.State != "active" || r.TargetPullNumber != targetPR {
				return routeStorageError()
			}
			if i == limit {
				result.HasMore = true
				break
			}
			result.Items = append(result.Items, FindingRouteSummary{ID: r.ID, Revision: r.Revision, SourceEvidenceID: r.SourceEvidenceID, Finding: r.Finding, SourcePullNumber: r.SourcePullNumber, TargetPullNumber: r.TargetPullNumber, Disposition: r.Disposition, HasFix: r.FixSHA != "", CreatedAt: r.CreatedAt})
		}
		if result.HasMore {
			last := result.Items[len(result.Items)-1]
			result.Cursor = e.store.Cursor(scope, filter, last.CreatedAt+"|"+last.ID)
		}
		return nil
	})
	if err != nil {
		return FindingRoutesPage{}, err
	}
	return result, nil
}

// pendingIncomingRoutes validates every active route before counting unresolved
// obligations, including apparently resolved rows. More than 100 is explicit
// capacity, never an unbounded scan or a false empty/passing obligation set.
func pendingIncomingRoutes(ctx context.Context, tx *sql.Tx, project string, pull int64) (int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM review_finding_routes WHERE project_id=? AND target_pull_number=? AND state='active' ORDER BY created_at DESC,id DESC LIMIT 101`, project, pull)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return 0, err
	}
	if len(ids) > 100 {
		return 0, core.Fail("review_capacity", "The receiving PR has more active routes than the bounded assessment can inspect.", 409)
	}
	var pending int64
	for _, id := range ids {
		route, err := readFindingRoute(ctx, tx, project, id)
		if err != nil {
			return 0, err
		}
		if route.State != "active" || route.TargetPullNumber != pull {
			return 0, routeStorageError()
		}
		if route.Disposition == "unadjudicated" || route.Disposition == "accepted" && route.FixSHA == "" {
			pending++
		}
	}
	return pending, nil
}
