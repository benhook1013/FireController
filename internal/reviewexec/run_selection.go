package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
	"math"
	"slices"
	"time"
)

type RunRequest struct {
	Channel                       string `json:"channel"`
	ExpectedPullNumber            *int64 `json:"expected_pull_number,omitempty"`
	ExpectedConfigurationRevision *int64 `json:"expected_configuration_revision,omitempty"`
}
type Run struct {
	ID                    string `json:"id"`
	ProjectID             string `json:"project_id"`
	ActorID               string `json:"actor_id"`
	WorkerID              string `json:"worker_id"`
	Channel               string `json:"channel"`
	ConfigurationRevision int64  `json:"configuration_revision"`
	PolicyRevision        int64  `json:"policy_revision"`
	CapabilityFingerprint string `json:"-"`
	ExpectedPullNumber    int64  `json:"expected_pull_number"`
	State                 string `json:"state"`
	Reason                string `json:"reason"`
	Revision              int64  `json:"revision"`
	OperationID           string `json:"operation_id,omitempty"`
	CreatedAt             string `json:"created_at"`
	UpdatedAt             string `json:"updated_at"`
}

const runColumns = `id,project_id,actor_id,worker_id,channel,configuration_revision,policy_revision,capability_fingerprint,expected_pull_number,state,reason,revision,COALESCE(operation_id,''),created_at,updated_at`

func validRunID(id string) bool    { return hostedObservationID(id, "review_selection_") }
func runPending(state string) bool { return state == "queued" || state == "selecting" }
func runUnavailable() error {
	return core.Fail("storage_unavailable", "Stored review selection is unavailable.", 500)
}
func validRun(r Run) bool {
	created, e1 := time.Parse(time.RFC3339Nano, r.CreatedAt)
	updated, e2 := time.Parse(time.RFC3339Nano, r.UpdatedAt)
	if !validRunID(r.ID) || !identifier.MatchString(r.ProjectID) || !identifier.MatchString(r.ActorID) || !identifier.MatchString(r.WorkerID) || !validChannel(r.Channel) || r.ConfigurationRevision < 1 || r.PolicyRevision < 1 || !hash.MatchString(r.CapabilityFingerprint) || r.ExpectedPullNumber < 0 || r.ExpectedPullNumber > 2147483647 || r.Revision < 1 || len(r.CreatedAt) > 64 || len(r.UpdatedAt) > 64 || e1 != nil || e2 != nil || created.IsZero() || updated.Before(created) {
		return false
	}
	switch r.State {
	case "queued", "selecting":
		return r.OperationID == "" && r.Reason == ""
	case "selected":
		return identifier.MatchString(r.OperationID) && r.Reason == ""
	case "held":
		return r.OperationID == "" && identifier.MatchString(r.Reason)
	case "empty":
		return r.OperationID == "" && r.Reason == "queue_complete"
	}
	return false
}
func readRun(ctx context.Context, q EvidenceQuery, project, id string) (Run, error) {
	var r Run
	err := q.QueryRowContext(ctx, `SELECT `+runColumns+` FROM review_selections WHERE project_id=? AND id=?`, project, id).Scan(&r.ID, &r.ProjectID, &r.ActorID, &r.WorkerID, &r.Channel, &r.ConfigurationRevision, &r.PolicyRevision, &r.CapabilityFingerprint, &r.ExpectedPullNumber, &r.State, &r.Reason, &r.Revision, &r.OperationID, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, core.Fail("not_found", "The review selection is unavailable in this project.", 404)
	}
	if err != nil {
		return r, err
	}
	if !validRun(r) {
		return Run{}, runUnavailable()
	}
	if r.State == "selected" {
		child, err := readOperation(ctx, q, r.ProjectID, r.OperationID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Run{}, runUnavailable()
			}
			var failure *core.Error
			if errors.As(err, &failure) && failure.Code == "not_found" {
				return Run{}, runUnavailable()
			}
			return Run{}, err
		}
		if child.ActorID != r.ActorID || child.WorkerID != r.WorkerID || child.Channel != r.Channel || child.ConfigurationRevision != r.ConfigurationRevision || child.PolicyRevision != r.PolicyRevision || child.CapabilityFingerprint != r.CapabilityFingerprint || r.ExpectedPullNumber != 0 && child.PullNumber != r.ExpectedPullNumber {
			return Run{}, runUnavailable()
		}
	}
	return r, nil
}
func (e *Engine) ReadRun(ctx context.Context, scope core.Scope, id string) (Run, error) {
	if !validRunID(id) {
		return Run{}, invalid()
	}
	var r Run
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		r, err = readRun(ctx, tx, scope.ProjectID, id)
		return err
	})
	return r, err
}
func (e *Engine) EnqueueRun(ctx context.Context, scope core.Scope, key string, input RunRequest) (json.RawMessage, error) {
	if !validChannel(input.Channel) || !identifier.MatchString(scope.WorkerID) || input.ExpectedPullNumber != nil && (*input.ExpectedPullNumber < 1 || *input.ExpectedPullNumber > 2147483647) || input.ExpectedConfigurationRevision != nil && (*input.ExpectedConfigurationRevision < 1 || *input.ExpectedConfigurationRevision == math.MaxInt64) {
		return nil, invalid()
	}
	if input.ExpectedPullNumber != nil {
		v := *input.ExpectedPullNumber
		input.ExpectedPullNumber = &v
	}
	if input.ExpectedConfigurationRevision != nil {
		v := *input.ExpectedConfigurationRevision
		input.ExpectedConfigurationRevision = &v
	}
	out, err := e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.run", Target: "channel-selection", Key: key, Request: input}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		c, revision, err := readConfiguration(ctx, tx, scope.ProjectID)
		if err != nil {
			return nil, err
		}
		if input.ExpectedConfigurationRevision != nil && *input.ExpectedConfigurationRevision != revision {
			return nil, conflict()
		}
		if c.Paused {
			return nil, core.Fail("reviews_paused", "Review execution is paused for this project.", 409)
		}
		if !slices.Contains(c.Channels, input.Channel) {
			return nil, core.Fail("review_not_configured", "The selected PR or channel is not configured.", 409)
		}
		if e.validateConfiguration(c) != nil {
			return nil, core.Fail("capability_unavailable", "The configured review capability is unavailable.", 409)
		}
		policy, err := readPolicy(ctx, tx, scope.ProjectID)
		if err != nil {
			return nil, err
		}
		if policy.Revision < 1 {
			return nil, core.Fail("reviews_policy_unconfigured", "Configure project review requirements before selecting a review.", 409)
		}
		if slices.Contains(policy.Policy.PausedChannels, input.Channel) {
			return nil, core.Fail("reviews_paused", "The requested review channel is paused.", 409)
		}
		var busy bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_selections WHERE project_id=? AND channel=? AND state IN ('queued','selecting'))`, scope.ProjectID, input.Channel).Scan(&busy); err != nil {
			return nil, err
		}
		if busy {
			return nil, core.Fail("review_busy", "A selection in this channel is already pending.", 409)
		}
		var total, local int64
		if err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(project_id=?),0) FROM review_selections WHERE state IN ('queued','selecting')`, scope.ProjectID).Scan(&total, &local); err != nil {
			return nil, err
		}
		if local >= 32 || total >= 256 {
			return nil, core.Fail("overloaded", "Review selection capacity is occupied.", 503)
		}
		expected := int64(0)
		if input.ExpectedPullNumber != nil {
			expected = *input.ExpectedPullNumber
		}
		id, at := core.NewID("review_selection"), stamp()
		_, err = tx.ExecContext(ctx, `INSERT INTO review_selections(id,project_id,actor_id,worker_id,channel,configuration_revision,policy_revision,capability_fingerprint,expected_pull_number,state,reason,revision,operation_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,'queued','',1,NULL,?,?)`, id, scope.ProjectID, scope.PrincipalID, scope.WorkerID, input.Channel, revision, policy.Revision, e.capabilities[c.CapabilityID].Fingerprint, expected, at, at)
		if err != nil {
			return nil, err
		}
		if err = core.AppendEvent(ctx, tx, scope, "review.selection_requested", id, 1, map[string]any{"channel": input.Channel}); err != nil {
			return nil, err
		}
		return Receipt{ID: id, State: "queued", Revision: 1}, nil
	})
	if err == nil {
		select {
		case e.runWake <- struct{}{}:
		default:
		}
	}
	return out, err
}
func (e *Engine) runBinding(ctx context.Context, tx *sql.Tx, r Run) (Configuration, int64, string, error) {
	c, revision, err := readConfiguration(ctx, tx, r.ProjectID)
	if err != nil {
		return c, 0, "", err
	}
	if revision != r.ConfigurationRevision || c.Paused || !slices.Contains(c.Channels, r.Channel) || e.validateConfiguration(c) != nil || e.capabilities[c.CapabilityID].Fingerprint != r.CapabilityFingerprint {
		return c, 0, "configuration_changed", nil
	}
	allowed, err := actorAllowed(ctx, tx, r.ProjectID, r.ActorID, r.WorkerID)
	if err != nil {
		return c, 0, "", err
	}
	if !allowed {
		return c, 0, "authorization_changed", nil
	}
	policy, err := readPolicy(ctx, tx, r.ProjectID)
	if err != nil {
		return c, 0, "", err
	}
	if policy.Revision != r.PolicyRevision || slices.Contains(policy.Policy.PausedChannels, r.Channel) {
		return c, 0, "policy_changed", nil
	}
	var repository int64
	if err = tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, r.ProjectID).Scan(&repository); err != nil {
		return c, 0, "", err
	}
	if repository < 1 {
		return c, 0, "repository_changed", nil
	}
	var busy bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_operations WHERE project_id=? AND channel=? AND state IN ('queued','preparing','posting','active','uncertain'))`, r.ProjectID, r.Channel).Scan(&busy); err != nil {
		return c, 0, "", err
	}
	if busy {
		return c, repository, "review_inflight", nil
	}
	return c, repository, "", nil
}
func updateRun(ctx context.Context, tx *sql.Tx, r Run, state, reason, operation string) error {
	return updateRunAt(ctx, tx, r, state, reason, operation, time.Now())
}
func updateRunAt(ctx context.Context, tx *sql.Tx, r Run, state, reason, operation string, now time.Time) error {
	if r.Revision == math.MaxInt64 {
		return runUnavailable()
	}
	old, _ := time.Parse(time.RFC3339Nano, r.UpdatedAt)
	if now.Before(old) {
		now = old
	}
	at := now.UTC().Format(time.RFC3339Nano)
	var link any
	if operation != "" {
		link = operation
	}
	res, err := tx.ExecContext(ctx, `UPDATE review_selections SET state=?,reason=?,operation_id=?,revision=revision+1,updated_at=? WHERE project_id=? AND id=? AND revision=? AND state=?`, state, reason, link, at, r.ProjectID, r.ID, r.Revision, r.State)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return conflict()
	}
	return core.AppendEvent(ctx, tx, core.Scope{ProjectID: r.ProjectID, PrincipalID: r.ActorID, WorkerID: r.WorkerID}, "review.selection_updated", r.ID, r.Revision+1, map[string]any{"state": state, "reason": reason})
}

// RecoverReviewRuns resets only read-only unfinished selection. Linked native
// actions are terminal selection rows and are never reselected or duplicated.
func (e *Engine) RecoverReviewRuns(ctx context.Context) error {
	var changed []string
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT project_id,id FROM review_selections WHERE state='selecting' ORDER BY id LIMIT 257`)
		if err != nil {
			return err
		}
		type key struct{ p, id string }
		var keys []key
		for rows.Next() {
			var k key
			if err = rows.Scan(&k.p, &k.id); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, k)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(keys) > 256 {
			return runUnavailable()
		}
		for _, k := range keys {
			r, err := readRun(ctx, tx, k.p, k.id)
			if err != nil {
				return err
			}
			if err = updateRun(ctx, tx, r, "queued", "", ""); err != nil {
				return err
			}
			changed = append(changed, r.ID)
		}
		return nil
	})
	if err == nil {
		for _, id := range changed {
			e.notifyOperation(id)
		}
	}
	return err
}

// WaitRun shares the finite operation-wait subscription capacity; every read
// reauthorizes, and cancellation detaches without canceling accepted work.
func (e *Engine) WaitRun(ctx context.Context, scope core.Scope, id string, seconds int) (Run, bool, error) {
	if ctx == nil || !validRunID(id) || !identifier.MatchString(scope.PrincipalID) || seconds < 0 || seconds > 900 {
		return Run{}, false, invalid()
	}
	read := func() (Run, error) {
		r, err := e.ReadRun(ctx, scope, id)
		if ctx.Err() != nil {
			return Run{}, core.Fail("overloaded", "The accepted selection wait was canceled; its request is retained.", 503)
		}
		return r, err
	}
	if seconds == 0 {
		r, err := read()
		return r, err == nil && !runPending(r.State), err
	}
	waiter := &operationWaiter{principal: scope.PrincipalID, changed: make(chan struct{}, 1)}
	e.operationMu.Lock()
	if e.operationWaiterCount >= 256 || e.operationWaiterPrincipals[waiter.principal] >= 8 {
		e.operationMu.Unlock()
		return Run{}, false, &core.Error{Code: "overloaded", Message: "Too many review result waits are open; read the retained selection.", Status: 503, Details: map[string]string{"reason": "operation_wait_capacity"}}
	}
	if e.operationWaiters == nil {
		e.operationWaiters = map[string]map[*operationWaiter]struct{}{}
		e.operationWaiterPrincipals = map[string]int{}
	}
	if e.operationWaiters[id] == nil {
		e.operationWaiters[id] = map[*operationWaiter]struct{}{}
	}
	e.operationWaiters[id][waiter] = struct{}{}
	e.operationWaiterCount++
	e.operationWaiterPrincipals[waiter.principal]++
	e.operationMu.Unlock()
	defer func() {
		e.operationMu.Lock()
		defer e.operationMu.Unlock()
		delete(e.operationWaiters[id], waiter)
		if len(e.operationWaiters[id]) == 0 {
			delete(e.operationWaiters, id)
		}
		e.operationWaiterCount--
		e.operationWaiterPrincipals[waiter.principal]--
		if e.operationWaiterPrincipals[waiter.principal] == 0 {
			delete(e.operationWaiterPrincipals, waiter.principal)
		}
	}()
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	for {
		r, err := read()
		if err != nil {
			return Run{}, false, err
		}
		if !runPending(r.State) {
			return r, true, nil
		}
		select {
		case <-ctx.Done():
			return Run{}, false, core.Fail("overloaded", "The accepted selection wait was canceled; its request is retained.", 503)
		case <-timer.C:
			r, err = read()
			return r, err == nil && !runPending(r.State), err
		case <-waiter.changed:
		}
	}
}

// AbandonReviewRuns participates in the offline provider-release transaction;
// historical actor provenance remains valid even after restore revocation.
func AbandonReviewRuns(ctx context.Context, tx *sql.Tx, at time.Time) (int64, error) {
	if ctx == nil || tx == nil || at.IsZero() {
		return 0, runUnavailable()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	present, err := restoredDomain(ctx, tx, "review_selection", "review_selections", "1")
	if err != nil || !present {
		return 0, err
	}
	if at.IsZero() {
		return 0, runUnavailable()
	}
	if err = ValidateSelectionSnapshot(ctx, tx); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT project_id,id FROM review_selections WHERE state IN ('queued','selecting') ORDER BY id LIMIT 10001`)
	if err != nil {
		return 0, err
	}
	type key struct{ project, id string }
	var selected []key
	for rows.Next() {
		var k key
		if err = rows.Scan(&k.project, &k.id); err != nil {
			rows.Close()
			return 0, err
		}
		selected = append(selected, k)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if len(selected) > 10000 {
		return 0, runUnavailable()
	}
	for _, k := range selected {
		r, err := readRun(ctx, tx, k.project, k.id)
		if err != nil {
			return 0, err
		}
		if err = updateRunAt(ctx, tx, r, "held", "restore_abandoned", "", at); err != nil {
			return 0, err
		}
	}
	return int64(len(selected)), nil
}
