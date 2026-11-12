package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
)

const queuedDiagnosticHeader = "X-FC-Review-Queued"
const queuedReservationHeader = "X-FC-Review-Reservation"

type queuedReservation struct {
	Kind        string `json:"kind"`
	OperationID string `json:"operation_id,omitempty"`
	PullNumber  int64  `json:"pull_number,omitempty"`
	Channel     string `json:"channel,omitempty"`
	State       string `json:"state,omitempty"`
}

// A best-effort current scheduling hint, never retained journal state or
// admission authority. Drift or unavailable facts omit the header, not the
// original response/receipt. The exact operation is reauthorized and compared
// with the response revision before consulting any current scheduling facts.
func (e *Engine) queuedResponseHeader(w http.ResponseWriter, r *http.Request, scope core.Scope, id, state, channel string, revision int64) {
	if state != "queued" {
		return
	}
	cause, reservation := e.queuedDiagnosticDetails(r.Context(), scope, id, revision)
	if cause == "queue_order" && channel == "cli" && r.Header.Get(operationWaitHeader) != "2" && r.Header.Get(operationWaitHeader) != "3" && r.Header.Get(operationWaitHeader) != "4" && r.Header.Get(operationWaitHeader) != "5" {
		cause = "awaiting_scheduler"
	}
	if cause != "" {
		w.Header().Set(queuedDiagnosticHeader, cause)
	}
	if reservation != nil {
		if body, err := json.Marshal(reservation); err == nil && len(body) <= 512 {
			w.Header().Set(queuedReservationHeader, string(body))
		}
	}
}
func (e *Engine) queuedDiagnostic(ctx context.Context, scope core.Scope, id string, revision int64) string {
	cause, _ := e.queuedDiagnosticDetails(ctx, scope, id, revision)
	return cause
}

func (e *Engine) queuedDiagnosticDetails(ctx context.Context, scope core.Scope, id string, revision int64) (string, *queuedReservation) {
	cause := ""
	var reservation *queuedReservation
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		op, err := readOperation(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return err
		}
		if op.ID != id || op.State != "queued" || op.Revision != revision {
			return nil
		}
		if err := e.requireAutomaticOperation(ctx, tx, op); err != nil {
			var failure *core.Error
			if errors.As(err, &failure) && failure.Code == "automation_authorization_changed" {
				cause = "automation_authorization_changed"
				return nil
			}
			return err
		}
		var originalJSON, latestJSON sql.NullString
		var repositoryID, latestRevision int64
		err = tx.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(c.body_json AS BLOB))<=32768 THEN c.body_json END,CASE WHEN length(CAST(latest.body_json AS BLOB))<=32768 THEN latest.body_json END,p.github_repository_id,latest.revision FROM review_execution_config c JOIN projects p ON p.id=c.project_id JOIN review_execution_config latest ON latest.project_id=c.project_id AND latest.revision=(SELECT MAX(revision) FROM review_execution_config WHERE project_id=c.project_id) WHERE c.project_id=? AND c.revision=?`, op.ProjectID, op.ConfigurationRevision).Scan(&originalJSON, &latestJSON, &repositoryID, &latestRevision)
		if err != nil {
			return err
		}
		var original, latest Configuration
		if !originalJSON.Valid || !latestJSON.Valid || json.Unmarshal([]byte(originalJSON.String), &original) != nil || json.Unmarshal([]byte(latestJSON.String), &latest) != nil || validateConfigurationShape(original) != nil || validateConfigurationShape(latest) != nil {
			return adjudicationStorageError()
		}
		switch {
		case op.Wait != nil && op.Wait.Paused:
			cause = "operation_paused"
			return nil
		case latest.Paused:
			cause = "project_paused"
			return nil
		}
		scheduling, err := readPullScheduling(ctx, tx, op.ProjectID, op.PullNumber)
		if err != nil {
			return err
		}
		if scheduling.Paused {
			cause = "pull_scheduling_paused"
			return nil
		}
		if op.Reason == cliDraftWait {
			cause = cliDraftWait
			return nil
		}
		if op.Channel == "cli" {
			compatible, err := compatibleQueuedConfiguration(ctx, tx, op, latest, latestRevision)
			if err != nil {
				return err
			}
			if capability, ok := e.capabilities[latest.CapabilityID]; compatible && ok && e.validateConfiguration(latest) == nil && capability.Fingerprint == op.CapabilityFingerprint {
				now := e.sampleExecutionTime()
				due, err := cliAccountRetryDeadline(ctx, tx, capability, now)
				if err != nil {
					return err
				}
				if now.Before(due) {
					cause = "cli_quota_wait"
					return nil
				}
			}
		}
		earlier, err := e.earlierQueuedReviewIntent(ctx, tx, op, latest, latestRevision)
		if err != nil {
			return err
		}
		if earlier != nil {
			cause = "queue_order"
			return nil
		}
		if op.Wait != nil && op.Wait.RetryNotBefore != "" && e.sampleExecutionTime().Before(mustWaitTime(op.Wait.RetryNotBefore)) {
			cause = "cooldown"
			return nil
		}
		cause = "awaiting_scheduler"
		capability, ok := e.capabilities[original.CapabilityID]
		if !ok || capability.Fingerprint != op.CapabilityFingerprint {
			cause = ""
			return nil
		}
		reservation, err = queuedReservationHolder(ctx, tx, op, capability, repositoryID)
		if reservation != nil {
			cause = "reservation_busy"
		}
		return err
	})
	if err != nil {
		return "", nil
	}
	return cause, reservation
}

// One matching reservation is enough to explain waiting. Joined current identity
// must agree before naming a same-project holder; no age-based stale inference
// or foreign operation facts leave this read. Hosted account exclusivity is per
// repository, while CLI retains the shared account boundary.
func queuedReservationHolder(ctx context.Context, tx *sql.Tx, waiting Operation, capability Capability, repositoryID int64) (*queuedReservation, error) {
	var provider, account, channel, attemptID string
	var repo, pull int64
	var aOperation, aProject, aProvider, aAccount, aState, anchor sql.NullString
	var oID, oProject, oAttempt, oChannel, oState, oHead sql.NullString
	var aRevision, oPull, oRepo sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT r.provider,r.account,r.action_class,r.repository_id,r.pull_number,r.attempt_id,
 a.operation_id,a.project_id,a.provider,a.account,a.state,a.revision,
 CASE WHEN length(CAST(a.anchor_json AS BLOB))<=2048 THEN a.anchor_json END,
 o.id,o.project_id,o.attempt_id,o.channel,o.state,o.head_sha,o.pull_number,p.github_repository_id
 FROM review_execution_reservations r
 LEFT JOIN review_execution_attempts a ON a.id=r.attempt_id
 LEFT JOIN review_operations o ON o.id=a.operation_id
 LEFT JOIN projects p ON p.id=o.project_id
 WHERE r.provider=? AND r.action_class=? AND ((r.account=? AND (?='cli' OR r.repository_id=?)) OR (r.repository_id=? AND r.pull_number=?))
 ORDER BY r.attempt_id LIMIT 1`, capability.Provider, waiting.Channel, capability.Account, waiting.Channel, repositoryID, repositoryID, waiting.PullNumber).Scan(&provider, &account, &channel, &repo, &pull, &attemptID, &aOperation, &aProject, &aProvider, &aAccount, &aState, &aRevision, &anchor, &oID, &oProject, &oAttempt, &oChannel, &oState, &oHead, &oPull, &oRepo)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	unproven := &queuedReservation{Kind: "reservation_unproven"}
	if repo < 1 || pull < 1 || pull > 2147483647 || !sha.MatchString(oHead.String) || !hostedObservationID(attemptID, "review_attempt_") || !hostedObservationID(oID.String, "review_operation_") || oID.String == waiting.ID || aOperation.String != oID.String || aProject.String == "" || aProject.String != oProject.String || aProvider.String != provider || aAccount.String != account || oAttempt.String != attemptID || oChannel.String != channel || oPull.Int64 != pull || oRepo.Int64 != repo || aRevision.Int64 < 1 || aState.String != oState.String || !reservationLiveState(aState.String) || !anchor.Valid {
		return unproven, nil
	}
	if anchor.String == "null" {
		if aState.String != "preparing" {
			return unproven, nil
		}
	} else {
		var admitted Anchor
		if json.Unmarshal([]byte(anchor.String), &admitted) != nil || encoded(admitted) != anchor.String || !validEvidenceAnchor(admitted) || admitted.RepositoryID != repo || admitted.PullNumber != pull || admitted.CandidateSHA != oHead.String {
			return unproven, nil
		}
	}
	if oProject.String != waiting.ProjectID {
		return &queuedReservation{Kind: "other_project"}, nil
	}
	return &queuedReservation{Kind: "same_project", OperationID: oID.String, PullNumber: pull, Channel: channel, State: aState.String}, nil
}

func reservationLiveState(state string) bool {
	return state == "preparing" || state == "posting" || state == "active" || state == "uncertain"
}
func (e *Engine) queuedRunResponseHeader(w http.ResponseWriter, r *http.Request, scope core.Scope, state RunState) {
	if state.Operation != nil {
		o := state.Operation
		e.queuedResponseHeader(w, r, scope, o.ID, o.State, o.Channel, o.Revision)
	}
}
