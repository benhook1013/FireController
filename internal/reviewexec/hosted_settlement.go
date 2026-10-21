package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
)

type attemptSettlementRequest struct {
	ExpectedRevision      *int64  `json:"expected_revision"`
	ExpectedRepositoryID  *int64  `json:"expected_repository_id"`
	ExpectedHeadSHA       *string `json:"expected_head_sha"`
	ExpectedExternalID    *string `json:"expected_external_id"`
	ExternalActionStopped *bool   `json:"external_action_stopped"`
	Reason                *string `json:"reason"`
	EvidenceReference     *string `json:"evidence_reference"`
}
type attemptSettlementReceipt struct {
	AttemptID   string `json:"attempt_id"`
	OperationID string `json:"operation_id"`
	Revision    int64  `json:"revision"`
	State       string `json:"state"`
	Outcome     string `json:"outcome"`
}
type attemptSettlementAudit struct {
	AttemptID             string `json:"attempt_id"`
	OperationID           string `json:"operation_id"`
	ExpectedRevision      int64  `json:"expected_revision"`
	Provider              string `json:"provider"`
	Account               string `json:"account"`
	Anchor                Anchor `json:"anchor"`
	ExternalID            string `json:"external_id"`
	ExternalActionStopped bool   `json:"external_action_stopped"`
	Reason                string `json:"reason"`
	EvidenceReference     string `json:"evidence_reference"`
}

func settlementUnavailable() error {
	return core.Fail("review_settlement_unavailable", "Only an eligible uncertain attempt with its original retained evidence and reservation can be settled.", 409)
}
func validSettlementText(value string, multiline bool) bool {
	return utf8.ValidString(value) && len(value) <= 1024 && strings.TrimSpace(value) != "" && strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) && (!multiline || r != '\t' && r != '\n') }) < 0
}

// This action records an administrator's independently verified external stop.
// It never fetches the reference, cancels a provider or fabricates completion.
func (e *Engine) settleAttemptHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.AdminScope(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var request attemptSettlementRequest
	if err := core.ReadJSON(w, r, &request); err != nil {
		core.WriteError(w, err)
		return
	}
	id := r.PathValue("attempt_id")
	if !hostedObservationID(id, "review_attempt_") || request.ExpectedRevision == nil || *request.ExpectedRevision < 1 || *request.ExpectedRevision == math.MaxInt64 || request.ExpectedRepositoryID == nil || *request.ExpectedRepositoryID <= 0 || request.ExpectedHeadSHA == nil || !sha.MatchString(*request.ExpectedHeadSHA) || request.ExpectedExternalID == nil || *request.ExpectedExternalID != "" && !identifier.MatchString(*request.ExpectedExternalID) || request.ExternalActionStopped == nil || !*request.ExternalActionStopped || request.Reason == nil || !validSettlementText(*request.Reason, true) || request.EvidenceReference == nil || !validSettlementText(*request.EvidenceReference, false) {
		core.WriteError(w, invalid())
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	// AdminScope makes Mutate reauthorize admin access before both replay and
	// fresh execution, in the same transaction as the guarded local changes.
	receipt, err := e.store.Mutate(r.Context(), scope, core.Mutation{Operation: "reviews.hosted.settle", Target: id, Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		a, err := readAttempt(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return nil, err
		}
		if a.Revision != *request.ExpectedRevision {
			return nil, &core.Error{Code: "review_attempt_revision_conflict", Message: "Reload the attempt before settlement.", Status: 409, Details: map[string]int64{"current_revision": a.Revision}}
		}
		o, err := readOperation(ctx, tx, scope.ProjectID, a.OperationID)
		if err != nil {
			return nil, err
		}
		if a.ID != id || a.ProjectID != scope.ProjectID || o.ProjectID != scope.ProjectID || o.ID != a.OperationID || o.AttemptID != a.ID || a.State != "uncertain" || o.State != a.State || !validChannel(o.Channel) || a.Revision < 3 || o.Revision < 1 || o.Revision == math.MaxInt64 || !identifier.MatchString(a.Provider) || !identifier.MatchString(a.Account) || !hostedObservationID(o.ID, "review_operation_") || !hash.MatchString(o.CapabilityFingerprint) || a.Anchor == nil || !validEvidenceAnchor(*a.Anchor) || a.Anchor.RepositoryID != *request.ExpectedRepositoryID || a.Anchor.CandidateSHA != *request.ExpectedHeadSHA || a.ExternalID != *request.ExpectedExternalID || a.Anchor.PullNumber != o.PullNumber || a.Anchor.CandidateSHA != o.HeadSHA {
			return nil, settlementUnavailable()
		}
		var originalAnchor, currentExternal, currentRecordedAt, currentReason string
		if err := tx.QueryRowContext(ctx, `SELECT anchor_json FROM review_execution_attempts WHERE id=?`, a.ID).Scan(&originalAnchor); err != nil {
			return nil, err
		}
		if originalAnchor != encoded(a.Anchor) {
			return nil, settlementUnavailable()
		}
		if err := tx.QueryRowContext(ctx, `SELECT external_id,created_at,reason FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision).Scan(&currentExternal, &currentRecordedAt, &currentReason); errors.Is(err, sql.ErrNoRows) {
			return nil, settlementUnavailable()
		} else if err != nil {
			return nil, err
		}
		if currentExternal != a.ExternalID || currentReason != o.Reason {
			return nil, settlementUnavailable()
		}
		at, err := attemptRecordingStamp(ctx, tx, o, &a, e.sampleAttemptTime())
		if err != nil {
			return nil, err
		}
		if err := validateSettlementAdmission(ctx, tx, a, o, currentRecordedAt); err != nil {
			return nil, err
		}
		state, reason, event, err := settlementDisposition(ctx, tx, a, o)
		if err != nil {
			return nil, err
		}
		var provider, account, class string
		var repositoryID, pullNumber int64
		err = tx.QueryRowContext(ctx, `SELECT provider,account,action_class,repository_id,pull_number FROM review_execution_reservations WHERE attempt_id=?`, a.ID).Scan(&provider, &account, &class, &repositoryID, &pullNumber)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, settlementUnavailable()
		}
		if err != nil {
			return nil, err
		}
		if provider != a.Provider || account != a.Account || class != o.Channel || repositoryID != a.Anchor.RepositoryID || pullNumber != a.Anchor.PullNumber {
			return nil, settlementUnavailable()
		}
		audit := attemptSettlementAudit{AttemptID: a.ID, OperationID: o.ID, ExpectedRevision: a.Revision, Provider: a.Provider, Account: a.Account, Anchor: *a.Anchor, ExternalID: a.ExternalID, ExternalActionStopped: true, Reason: *request.Reason, EvidenceReference: *request.EvidenceReference}
		payload, err := json.Marshal(audit)
		if err != nil {
			return nil, invalid()
		}
		if len(payload) > 4096 {
			return nil, core.Fail("settlement_audit_too_large", "Shorten the operator reason or evidence reference to fit the settlement audit limit.", 400)
		}
		updated, err := tx.ExecContext(ctx, `UPDATE review_execution_attempts SET state=?,revision=revision+1 WHERE id=? AND project_id=? AND state='uncertain' AND revision=?`, state, a.ID, scope.ProjectID, a.Revision)
		if err != nil {
			return nil, err
		}
		if count, err := updated.RowsAffected(); err != nil {
			return nil, err
		} else if count != 1 {
			return nil, settlementUnavailable()
		}
		updated, err = tx.ExecContext(ctx, `UPDATE review_operations SET state=?,revision=revision+1,reason=?,updated_at=? WHERE id=? AND project_id=? AND state='uncertain' AND revision=? AND attempt_id=?`, state, reason, at, o.ID, scope.ProjectID, o.Revision, a.ID)
		if err != nil {
			return nil, err
		}
		if count, err := updated.RowsAffected(); err != nil {
			return nil, err
		} else if count != 1 {
			return nil, settlementUnavailable()
		}
		deleted, err := tx.ExecContext(ctx, `DELETE FROM review_execution_reservations WHERE attempt_id=? AND provider=? AND account=? AND action_class=? AND repository_id=? AND pull_number=?`, a.ID, a.Provider, a.Account, o.Channel, a.Anchor.RepositoryID, a.Anchor.PullNumber)
		if err != nil {
			return nil, err
		}
		if count, err := deleted.RowsAffected(); err != nil {
			return nil, err
		} else if count != 1 {
			return nil, settlementUnavailable()
		}
		a.State = state
		a.Revision++
		if err := e.transitionRow(ctx, tx, a, reason, at); err != nil {
			return nil, err
		}
		if err := core.AppendEvent(ctx, tx, scope, "review.operation_updated", o.ID, o.Revision+1, struct{}{}); err != nil {
			return nil, err
		}
		if err := core.AppendEvent(ctx, tx, scope, event, a.ID, a.Revision, audit); err != nil {
			return nil, err
		}
		return attemptSettlementReceipt{AttemptID: a.ID, OperationID: o.ID, Revision: a.Revision, State: state, Outcome: "settled"}, nil
	})
	if err != nil {
		core.WriteError(w, err)
		return
	}
	var settled attemptSettlementReceipt
	if json.Unmarshal(receipt, &settled) == nil {
		e.notifyOperation(settled.OperationID)
	}
	core.WriteJSON(w, 200, json.RawMessage(receipt))
}

func validateSettlementAdmission(ctx context.Context, tx *sql.Tx, a Attempt, o Operation, currentAt string) error {
	rows, err := tx.QueryContext(ctx, `SELECT revision,state,external_id,created_at FROM review_execution_transitions WHERE attempt_id=? AND reason='admitted' ORDER BY revision LIMIT 2`, a.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	created, createdOK := attemptRecordedTime(o.CreatedAt)
	current, currentOK := attemptRecordedTime(currentAt)
	expectedState := "posting"
	if o.Channel == "cli" {
		expectedState = "active"
	}
	for rows.Next() {
		var revision int64
		var state, external, recorded string
		if err := rows.Scan(&revision, &state, &external, &recorded); err != nil {
			return err
		}
		count++
		admitted, valid := attemptRecordedTime(recorded)
		if count != 1 || revision != 2 || state != expectedState || external != "" || !valid || !createdOK || !currentOK || admitted.Before(created) || admitted.After(current) {
			return settlementUnavailable()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count != 1 {
		return settlementUnavailable()
	}
	return nil
}

func validateSettlementObservations(ctx context.Context, tx *sql.Tx, a Attempt, o Operation, full bool) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,project_id,attempt_id,operation_id,position,capture_sha256,capture_bytes,body_json FROM review_hosted_observations WHERE attempt_id=? ORDER BY position LIMIT 201`, a.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := int64(0)
	ordinary := false
	for rows.Next() {
		var id, project, attempt, operation, checksum, body string
		var position, bytes int64
		if err := rows.Scan(&id, &project, &attempt, &operation, &position, &checksum, &bytes, &body); err != nil {
			return err
		}
		count++
		if count > hostedObservationLimit || position != count || project != a.ProjectID || attempt != a.ID || operation != o.ID {
			return settlementUnavailable()
		}
		record, err := decodeHostedObservation(body, id, project, attempt, operation, position, checksum, bytes, a, o)
		if err != nil {
			return settlementUnavailable()
		}
		if err := hostedObservationQueryBinding(ctx, tx, a, o, record.Input); err != nil {
			return settlementUnavailable()
		}
		ordinary = ordinary || record.Input.Kind == ""
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if full && count != hostedObservationLimit || !full && (count == 0 || !ordinary) {
		return settlementUnavailable()
	}
	return nil
}
