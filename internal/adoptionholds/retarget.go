package adoptionholds

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	m "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/core"
)

type rowScanner interface{ Scan(...any) error }

const retargetColumns = `revision,target_pull_number,CASE WHEN length(CAST(targets_json AS BLOB))<=2048 THEN targets_json END,CASE WHEN length(CAST(reason AS BLOB))<=4096 THEN reason END,actor_id,worker_id,changed_at`

func readRetargetRecord(row rowScanner, original m.Obligation) (m.Retarget, string, error) {
	var result m.Retarget
	var targets string
	var revisionValue, targetValue, targetsValue, reasonValue, actorValue, workerValue, changedValue any
	if err := row.Scan(&revisionValue, &targetValue, &targetsValue, &reasonValue, &actorValue, &workerValue, &changedValue); err != nil {
		return result, targets, err
	}
	if !retainedInteger(revisionValue, &result.Revision) || !retainedInteger(targetValue, &result.TargetPullNumber) || !retainedText(targetsValue, &targets) || !retainedText(reasonValue, &result.Reason) || !retainedText(actorValue, &result.ActorID) || !retainedText(workerValue, &result.WorkerID) || !retainedText(changedValue, &result.ChangedAt) || original.Kind != "route" || result.Revision < 2 || !identifier.MatchString(result.ActorID) || !identifier.MatchString(result.WorkerID) || !validTime(result.ChangedAt) {
		return result, targets, unavailable()
	}
	previous := result.Revision - 1
	if _, err := m.NormalizeRetarget(m.RetargetRequest{ExpectedRevision: &previous, TargetPullNumber: &result.TargetPullNumber, Reason: result.Reason}); err != nil || string(encode(m.RouteAffectedPullNumbers(original, result.TargetPullNumber))) != targets {
		return result, targets, unavailable()
	}
	return result, targets, nil
}

func readRetargetState(ctx context.Context, q Query, detail *m.Detail, version string) (int64, error) {
	if version == "1" {
		if !slices.Equal(detail.AffectedPullNumbers, detail.Original.AffectedPullNumbers) {
			return 0, unavailable()
		}
		return 1, nil
	}
	if version != "2" {
		return 0, unavailable()
	}
	var count, first, last int64
	var countValue, firstValue, lastValue any
	if err := q.QueryRowContext(ctx, `SELECT count(*),COALESCE(min(revision),1),COALESCE(max(revision),1) FROM review_adoption_retargets WHERE obligation_id=?`, detail.ID).Scan(&countValue, &firstValue, &lastValue); err != nil {
		return 0, err
	}
	if !retainedInteger(countValue, &count) || !retainedInteger(firstValue, &first) || !retainedInteger(lastValue, &last) {
		return 0, unavailable()
	}
	if count == 0 {
		if !slices.Equal(detail.AffectedPullNumbers, detail.Original.AffectedPullNumbers) {
			return 0, unavailable()
		}
		return 1, nil
	}
	if first != 2 || count != last-1 {
		return 0, unavailable()
	}
	latest, targets, err := readRetargetRecord(q.QueryRowContext(ctx, `SELECT `+retargetColumns+` FROM review_adoption_retargets WHERE obligation_id=? ORDER BY revision DESC LIMIT 1`, detail.ID), detail.Original)
	if err != nil {
		return 0, err
	}
	created, _ := time.Parse(time.RFC3339Nano, detail.CreatedAt)
	changed, _ := time.Parse(time.RFC3339Nano, latest.ChangedAt)
	if latest.Revision != last || targets != string(encode(detail.AffectedPullNumbers)) || changed.Before(created) {
		return 0, unavailable()
	}
	detail.Retarget = &latest
	return latest.Revision, nil
}

// Retarget changes a route's current receiver, never its imported source facts
// or review accounting. Mutate rechecks writer access on writes and key replay.
func (s *Service) Retarget(ctx context.Context, scope core.Scope, id, key string, input m.RetargetRequest) (json.RawMessage, error) {
	if scope.WorkerID == "" {
		return nil, core.Fail("forbidden", "Project worker attribution is required.", 403)
	}
	if !identifier.MatchString(id) {
		return nil, invalid()
	}
	request, err := m.NormalizeRetarget(input)
	if err != nil {
		return nil, invalid()
	}
	return s.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.adoption_obligations.retarget", Target: id, Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		current, err := readDetail(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return nil, err
		}
		if current.Revision != *request.ExpectedRevision || current.Status != "open" {
			return nil, conflict()
		}
		if current.Original.Kind != "route" {
			return nil, core.Fail("adoption_obligation_not_route", "Only an open routed obligation can change receiver.", 409)
		}
		prior := m.OriginalRouteTarget(current.Original)
		if current.Retarget != nil {
			prior = current.Retarget.TargetPullNumber
		}
		target := *request.TargetPullNumber
		numbers := m.RouteAffectedPullNumbers(current.Original, target)
		receipt := m.RetargetReceipt{ID: id, Status: "open", Revision: current.Revision, TargetPullNumber: target, AffectedPullNumbers: current.AffectedPullNumbers, Outcome: "unchanged"}
		if prior == target && slices.Equal(current.AffectedPullNumbers, numbers) {
			return receipt, nil
		}
		at := time.Now().UTC()
		previous := current.CreatedAt
		if current.Retarget != nil {
			previous = current.Retarget.ChangedAt
		}
		if recorded, err := time.Parse(time.RFC3339Nano, previous); err == nil && at.Before(recorded) {
			at = recorded
		}
		revision := current.Revision + 1
		result, err := tx.ExecContext(ctx, `UPDATE review_adoption_obligations SET revision=? WHERE project_id=? AND id=? AND status='open' AND revision=?`, revision, scope.ProjectID, id, current.Revision)
		if err != nil {
			return nil, err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			if err != nil {
				return nil, err
			}
			return nil, conflict()
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_adoption_retargets(obligation_id,revision,target_pull_number,targets_json,reason,actor_id,worker_id,changed_at) VALUES(?,?,?,?,?,?,?,?)`, id, revision, target, string(encode(numbers)), request.Reason, scope.PrincipalID, scope.WorkerID, at.Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM review_adoption_targets WHERE obligation_id=? AND project_id=? AND pull_number NOT IN(SELECT value FROM json_each(?))`, id, scope.ProjectID, string(encode(numbers))); err != nil {
			return nil, err
		}
		for _, number := range numbers {
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO review_adoption_targets(obligation_id,project_id,pull_number) VALUES(?,?,?)`, id, scope.ProjectID, number); err != nil {
				return nil, err
			}
		}
		if err = core.AppendEvent(ctx, tx, scope, "reviews.adoption_obligation_retargeted", id, revision, struct {
			TargetPullNumber int64 `json:"target_pull_number"`
		}{target}); err != nil {
			return nil, err
		}
		receipt.Revision, receipt.AffectedPullNumbers, receipt.Outcome = revision, numbers, "retargeted"
		return receipt, nil
	})
}

func (s *Service) retargetHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := s.store.Scope(r, "write")
	if err == nil {
		_, err = core.QueryKeys(r, "validation_failed")
	}
	if err != nil {
		core.WriteError(w, err)
		return
	}
	var input m.RetargetRequest
	if err := core.ReadJSON(w, r, &input); err != nil {
		core.WriteError(w, err)
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	out, err := s.Retarget(r.Context(), scope, r.PathValue("id"), key, input)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, 200, out)
}

// validateRetargetHistory walks bounded pages only during snapshot inspection.
func validateRetargetHistory(ctx context.Context, q Query, detail m.Detail) error {
	var version string
	if err := q.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:review_adoption_obligations'`).Scan(&version); err != nil || version == "1" {
		return err
	}
	after := int64(1)
	previous, _ := time.Parse(time.RFC3339Nano, detail.CreatedAt)
	for {
		rows, err := q.QueryContext(ctx, `SELECT `+retargetColumns+` FROM review_adoption_retargets WHERE obligation_id=? AND revision>? ORDER BY revision LIMIT 64`, detail.ID, after)
		if err != nil {
			return err
		}
		count := 0
		for rows.Next() {
			change, _, readErr := readRetargetRecord(rows, detail.Original)
			if readErr != nil {
				rows.Close()
				return readErr
			}
			changed, _ := time.Parse(time.RFC3339Nano, change.ChangedAt)
			if change.Revision != after+1 || changed.Before(previous) || detail.Resolution != nil && change.Revision >= detail.Resolution.Revision {
				rows.Close()
				return unavailable()
			}
			after, previous = change.Revision, changed
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if count < 64 {
			return nil
		}
	}
}
