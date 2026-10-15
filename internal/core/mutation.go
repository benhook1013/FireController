package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

func (s *Store) Mutate(ctx context.Context, scope Scope, m Mutation, fn func(context.Context, *sql.Tx) (any, error)) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, storageError(err)
	}
	fingerprint, err := mutationFingerprint(scope, m)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	changes := &eventChanges{topics: make(map[messageTopic]struct{})}
	ctx = context.WithValue(ctx, eventChangesKey{}, changes)
	select {
	case s.admission <- struct{}{}:
		defer func() { <-s.admission }()
	case <-ctx.Done():
		return nil, storageError(ctx.Err())
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return nil, mutationError(ctx, err)
	}
	defer tx.Rollback()
	response, found, err := s.replay(ctx, tx, scope, m, fingerprint)
	if ctx.Err() != nil {
		return nil, storageError(ctx.Err())
	}
	if err != nil || found {
		return response, err
	}
	value, err := fn(ctx, tx)
	if ctx.Err() != nil {
		return nil, storageError(ctx.Err())
	}
	if err != nil {
		return nil, mutationError(ctx, err)
	}
	encoded, err := json.Marshal(value)
	if ctx.Err() != nil {
		return nil, storageError(ctx.Err())
	}
	if err != nil {
		return nil, Fail("internal_error", "The mutation receipt could not be encoded.", 500)
	}
	if len(encoded) > 256<<10 {
		return nil, Fail("internal_error", "The mutation receipt exceeds its bounded size.", 500)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO receipts(instance_id,principal_id,project_id,operation,target,key,fingerprint,response_json) VALUES(?,?,?,?,?,?,?,?)", s.instanceID, scope.PrincipalID, scope.ProjectID, m.Operation, m.Target, m.Key, fingerprint, string(encoded)); err != nil {
		return nil, mutationError(ctx, err)
	}
	if ctx.Err() != nil {
		return nil, storageError(ctx.Err())
	}
	if err = tx.Commit(); err != nil {
		return nil, mutationError(ctx, err)
	}
	s.notifyEvents(changes)
	return encoded, nil
}

// The transaction deadline is authoritative even when the driver reports an
// interrupt or database/sql has already completed its cancellation rollback.
// Without cancellation, genuine storage failures retain their failure class.
func mutationError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return storageError(ctx.Err())
	}
	var coded interface{ Code() int }
	if errors.As(err, &coded) || errors.Is(err, sql.ErrTxDone) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return storageError(err)
	}
	return err
}

func mutationFingerprint(scope Scope, m Mutation) (string, error) {
	if !validIdempotencyKey(m.Key) || m.Operation == "" || m.Target == "" {
		return "", Fail("validation_failed", "A bounded Idempotency-Key, operation and target are required.", 400)
	}
	data, err := json.Marshal(struct {
		Request any
		Worker  string
	}{m.Request, scope.WorkerID})
	if err != nil {
		return "", Fail("validation_failed", "The mutation request cannot be encoded.", 400)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Replay checks current access and a retained receipt in one read transaction.
// It avoids a provider lookup on exact retries; new mutations still use Mutate.
func (s *Store) Replay(ctx context.Context, scope Scope, m Mutation) (json.RawMessage, bool, error) {
	fingerprint, err := mutationFingerprint(scope, m)
	if err != nil {
		return nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, storageError(err)
	}
	defer tx.Rollback()
	return s.replay(ctx, tx, scope, m, fingerprint)
}

func (s *Store) replay(ctx context.Context, tx *sql.Tx, scope Scope, m Mutation, fingerprint string) (json.RawMessage, bool, error) {
	permission := "write"
	if scope.admin {
		permission = "admin"
	}
	if err := s.authorize(ctx, tx, scope, permission); err != nil {
		return nil, false, err
	}
	var oldFingerprint, response string
	err := tx.QueryRowContext(ctx, "SELECT fingerprint,response_json FROM receipts WHERE instance_id=? AND principal_id=? AND project_id=? AND operation=? AND target=? AND key=?", s.instanceID, scope.PrincipalID, scope.ProjectID, m.Operation, m.Target, m.Key).Scan(&oldFingerprint, &response)
	if err == nil {
		if oldFingerprint != fingerprint {
			return nil, false, Fail("idempotency_conflict", "The mutation key was already used for a different request.", 409)
		}
		return json.RawMessage(response), true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, storageError(err)
	}
	return nil, false, nil
}

func AppendEvent(ctx context.Context, tx *sql.Tx, scope Scope, eventType, resourceID string, revision int64, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil || len(data) > 4096 {
		return Fail("validation_failed", "Event metadata must be compact JSON.", 400)
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO events(project_id,event_type,resource_id,revision,actor_id,payload_json,created_at) VALUES(?,?,?,?,?,?,?)", scope.ProjectID, eventType, resourceID, revision, scope.PrincipalID, string(data), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return storageError(err)
	}
	if changes, ok := ctx.Value(eventChangesKey{}).(*eventChanges); ok {
		if eventType == "credentials.revoked" {
			changes.revoked = true
		}
		if eventType == "message.sent" || eventType == "message.seen" || eventType == "message.acknowledged" {
			var recipient struct {
				Worker string `json:"recipient_worker_id"`
			}
			if json.Unmarshal(data, &recipient) == nil && recipient.Worker != "" {
				changes.topics[messageTopic{scope.ProjectID, recipient.Worker}] = struct{}{}
			}
		}
	}
	return nil
}

func PageLimit(r *http.Request) (int, error) {
	values, present := r.URL.Query()["limit"]
	if !present {
		return 20, nil
	}
	if len(values) != 1 {
		return 0, Fail("validation_failed", "Select one page limit between 1 and 100.", 400)
	}
	n, err := strconv.Atoi(values[0])
	if err != nil || n < 1 || n > 100 {
		return 0, Fail("validation_failed", "Select one page limit between 1 and 100.", 400)
	}
	return n, nil
}
