package reviews

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"strings"
	"time"
	"unicode"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/publicsite"
)

// NativeAdjudication projects authoritative ledger state inside its caller's
// serialized transaction. It cannot replace captured finding text or PR facts.
type NativeAdjudication struct {
	SourceRunID  string
	Finalized    bool
	FrozenCounts *NativeCounts
	Findings     []NativeFindingUpdate
}

type NativeCounts struct{ Found, Accepted, Routed int64 }

type NativeFindingUpdate struct {
	Route                                                          *FindingRoute
	SourceFindingID, Disposition, DecisionReason, Actor, DecidedAt string
	FixSHA, ProofNote, ResolvedAt                                  string
}

func nativeAdjudicationConflict() error {
	return core.Fail("observation_conflict", "Native adjudication cannot replace source identity, finalized counts or retained proof.", 409)
}

func nativeAdjudicationText(value string, maximum int, required bool) bool {
	return validText(value, maximum, required) && strings.IndexFunc(value, func(r rune) bool {
		return unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r'
	}) < 0
}

func nativeAdjudicationTime(value string) bool {
	parsed, err := parseTimestamp(value, true)
	return err == nil && !parsed.IsZero()
}

func validateNativeAdjudication(input NativeAdjudication) (map[string]NativeFindingUpdate, NativeCounts, int64, error) {
	if !nativeAdjudicationText(input.SourceRunID, 256, true) || len(input.Findings) > maxRunFindings || input.Finalized != (input.FrozenCounts != nil) {
		return nil, NativeCounts{}, 0, validationError("Use one bounded native source run and explicit adjudication state.")
	}
	updates := make(map[string]NativeFindingUpdate, len(input.Findings))
	counts := NativeCounts{Found: int64(len(input.Findings))}
	var fixed int64
	for _, finding := range input.Findings {
		if !nativeAdjudicationText(finding.SourceFindingID, 256, true) || !oneOf(finding.Disposition, "unadjudicated", "accepted", "routed", "rejected") || !nativeAdjudicationText(finding.DecisionReason, 2048, false) || !nativeAdjudicationText(finding.Actor, 128, false) || !nativeAdjudicationText(finding.ProofNote, 2048, false) {
			return nil, counts, 0, validationError("Use valid native finding identities, decisions and bounded proof.")
		}
		if finding.Disposition == "routed" && !validFindingRoute(finding.Route, 0, true) || finding.Disposition != "routed" && finding.Route != nil {
			return nil, counts, 0, validationError("Native routed findings require their active receiving route.")
		}
		if _, duplicate := updates[finding.SourceFindingID]; duplicate {
			return nil, counts, 0, validationError("Native finding identities must be unique.")
		}
		if finding.Disposition == "unadjudicated" {
			if input.Finalized || finding.DecisionReason != "" || finding.Actor != "" || finding.DecidedAt != "" || finding.FixSHA != "" || finding.ProofNote != "" || finding.ResolvedAt != "" {
				return nil, counts, 0, validationError("Undecided native findings cannot carry decisions or proof.")
			}
		} else if strings.TrimSpace(finding.DecisionReason) == "" || strings.TrimSpace(finding.Actor) == "" || !nativeAdjudicationTime(finding.DecidedAt) {
			return nil, counts, 0, validationError("Native decisions require a reason, actor and timestamp.")
		}
		if finding.FixSHA == "" {
			if finding.ProofNote != "" || finding.ResolvedAt != "" {
				return nil, counts, 0, validationError("Native proof requires its exact fix SHA.")
			}
		} else if finding.Disposition != "accepted" || len(finding.FixSHA) != 40 || strings.ToLower(finding.FixSHA) != finding.FixSHA || !validSHA(finding.FixSHA) || strings.TrimSpace(finding.ProofNote) == "" || !nativeAdjudicationTime(finding.ResolvedAt) {
			return nil, counts, 0, validationError("Native proof requires an accepted finding, full lowercase fix SHA, note and timestamp.")
		} else {
			fixed++
		}
		if finding.Disposition == "accepted" {
			counts.Accepted++
		} else if finding.Disposition == "routed" {
			counts.Routed++
		}
		updates[finding.SourceFindingID] = finding
	}
	if frozen := input.FrozenCounts; frozen != nil && (frozen.Found != counts.Found || frozen.Accepted < 0 || frozen.Routed < 0 || frozen.Accepted > frozen.Found-frozen.Routed) {
		return nil, counts, 0, validationError("Native frozen counts must match the immutable finding count and supported dispositions.")
	}
	return updates, counts, fixed, nil
}

// UpdateNativeAdjudication updates only an already projected native run. The
// caller owns authorization and ledger revisions in this same transaction.
// Missing projections and exact unchanged state produce no snapshot or event.
func UpdateNativeAdjudication(ctx context.Context, tx *sql.Tx, scope core.Scope, input NativeAdjudication) (Receipt, bool, error) {
	updates, decisions, fixed, err := validateNativeAdjudication(input)
	if err != nil {
		return Receipt{}, false, err
	}
	var revision, repositoryID, number, found, accepted, routed, oldFixed int64
	var previous, observed, runID, outcome string
	var finalized, complete bool
	err = tx.QueryRowContext(ctx, `SELECT w.revision,w.snapshot_id,s.repository_id,s.observed_at,r.pull_number,r.id,r.found,r.accepted,r.routed,r.finalized,r.fixed,r.complete,r.outcome
		FROM review_workspace w JOIN review_snapshots s ON s.project_id=w.project_id AND s.snapshot_id=w.snapshot_id
		JOIN review_runs r ON r.project_id=w.project_id AND r.snapshot_id=w.snapshot_id
		WHERE w.project_id=? AND r.source=? AND r.source_run_id=?`, scope.ProjectID, nativeSource, input.SourceRunID).Scan(&revision, &previous, &repositoryID, &observed, &number, &runID, &found, &accepted, &routed, &finalized, &oldFixed, &complete, &outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, err
	}
	current := Receipt{ID: previous, Revision: revision, Outcome: "observed"}
	rows, err := tx.QueryContext(ctx, `SELECT source_finding_id,disposition,decision_reason,actor,decided_at,fix_sha,proof_note,resolved_at,route_json FROM review_findings WHERE project_id=? AND snapshot_id=? AND run_id=? ORDER BY position`, scope.ProjectID, previous, runID)
	if err != nil {
		return Receipt{}, false, err
	}
	ordered := make([]NativeFindingUpdate, 0, len(input.Findings))
	changed := finalized != input.Finalized || oldFixed != fixed
	for rows.Next() {
		var before NativeFindingUpdate
		var routeJSON string
		if err = rows.Scan(&before.SourceFindingID, &before.Disposition, &before.DecisionReason, &before.Actor, &before.DecidedAt, &before.FixSHA, &before.ProofNote, &before.ResolvedAt, &routeJSON); err != nil {
			break
		}
		before.Route, err = decodeFindingRoute(routeJSON, number)
		if err != nil {
			break
		}
		next, exists := updates[before.SourceFindingID]
		if !exists || before.FixSHA != "" && (next.Disposition != "accepted" || next.FixSHA == "") || len(ordered) >= maxRunFindings {
			err = nativeAdjudicationConflict()
			break
		}
		if before.Route != nil && before.Route.FixSHA != "" && (next.Route == nil || next.Route.ID != before.Route.ID || next.Route.TargetPullNumber != before.Route.TargetPullNumber || next.Route.Disposition != "accepted" || next.Route.FixSHA == "") {
			err = nativeAdjudicationConflict()
			break
		}
		if next.Route != nil && !validFindingRoute(next.Route, number, true) {
			err = nativeAdjudicationConflict()
			break
		}
		changed = changed || !reflect.DeepEqual(before, next)
		ordered = append(ordered, next)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return Receipt{}, false, err
	}
	if len(ordered) != len(input.Findings) || found < int64(len(ordered)) {
		return Receipt{}, false, nativeAdjudicationConflict()
	}
	if finalized {
		if !input.Finalized || *input.FrozenCounts != (NativeCounts{Found: found, Accepted: accepted, Routed: routed}) {
			return Receipt{}, false, nativeAdjudicationConflict()
		}
	} else if input.Finalized {
		if !complete || outcome != "completed" || found != decisions.Found || *input.FrozenCounts != decisions {
			return Receipt{}, false, nativeAdjudicationConflict()
		}
		accepted, routed = decisions.Accepted, decisions.Routed
	} else {
		changed = changed || accepted != decisions.Accepted || routed != decisions.Routed
		accepted, routed = decisions.Accepted, decisions.Routed
	}
	if !changed {
		return current, false, nil
	}
	if revision == math.MaxInt64 {
		return Receipt{}, false, core.Fail("revision_exhausted", "The review workspace revision cannot advance further.", 409)
	}
	input.Findings = ordered
	digest := sha256.Sum256([]byte(previous + "\x00" + mustJSON(input)))
	id := core.NewID("review_snapshot")
	next := revision + 1
	recordedAt := time.Now().UTC().Format(time.RFC3339Nano)
	if err = preflightNativeAdjudicationDetail(ctx, tx, scope.ProjectID, previous, runID, snapshotMeta{ProjectID: scope.ProjectID, Revision: next, SnapshotID: id, Source: nativeSource, ObservedAt: observed, RecordedAt: recordedAt}, updates, accepted, routed, input.Finalized, fixed); err != nil {
		return Receipt{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO review_snapshots(project_id,snapshot_id,revision,repository_id,source,observed_at,recorded_at,payload_sha256) VALUES(?,?,?,?,?,?,?,?)`, scope.ProjectID, id, next, repositoryID, nativeSource, observed, recordedAt, hex.EncodeToString(digest[:])); err != nil {
		return Receipt{}, false, err
	}
	if err = copyNativeSnapshot(ctx, tx, scope.ProjectID, previous, id); err != nil {
		return Receipt{}, false, err
	}
	for _, finding := range ordered {
		if _, err = tx.ExecContext(ctx, `UPDATE review_findings SET disposition=?,decision_reason=?,actor=?,decided_at=?,fix_sha=?,proof_note=?,resolved_at=?,route_json=? WHERE project_id=? AND snapshot_id=? AND run_id=? AND source_finding_id=?`, finding.Disposition, finding.DecisionReason, finding.Actor, finding.DecidedAt, finding.FixSHA, finding.ProofNote, finding.ResolvedAt, mustJSON(finding.Route), scope.ProjectID, id, runID, finding.SourceFindingID); err != nil {
			return Receipt{}, false, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE review_runs SET accepted=?,routed=?,finalized=?,fixed=? WHERE project_id=? AND snapshot_id=? AND id=? AND source=?`, accepted, routed, input.Finalized, fixed, scope.ProjectID, id, runID, nativeSource); err != nil {
		return Receipt{}, false, err
	}
	if err = checkNativeCapacity(ctx, tx, scope.ProjectID, id); err != nil {
		return Receipt{}, false, err
	}
	if err = checkNativeRunDetail(ctx, tx, scope.ProjectID, id, runID); err != nil {
		return Receipt{}, false, err
	}
	if err = rebuildNativeSummaries(ctx, tx, scope.ProjectID, id, number); err != nil {
		return Receipt{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE review_workspace SET revision=?,snapshot_id=? WHERE project_id=? AND revision=? AND snapshot_id=?`, next, id, scope.ProjectID, revision, previous)
	if err != nil {
		return Receipt{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Receipt{}, false, err
	}
	if affected != 1 {
		return Receipt{}, false, nativeAdjudicationConflict()
	}
	if err = core.AppendEvent(ctx, tx, scope, "review.observed", id, next, map[string]any{"snapshot_id": id, "pull_number": number, "source": nativeSource}); err != nil {
		return Receipt{}, false, err
	}
	if err = publicsite.Refresh(ctx, tx, scope.ProjectID, PublicSiteReviewFacts); err != nil {
		return Receipt{}, false, err
	}
	return Receipt{ID: id, Revision: next, Outcome: "observed"}, true, nil
}
