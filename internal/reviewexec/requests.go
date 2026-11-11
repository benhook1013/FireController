package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"

	"github.com/benhook1013/FireController/internal/core"
)

func validateConfigurationShape(c Configuration) error {
	if !identifier.MatchString(c.CapabilityID) || c.ExpectedRevision == nil || *c.ExpectedRevision < 0 || c.Mode != "independent" && c.Mode != "stacked" || len(c.Queue) == 0 || len(c.Queue) > 100 || len(c.Channels) == 0 || len(c.Channels) > 2 {
		return invalid()
	}
	numbers := map[int64]bool{}
	for _, n := range c.Queue {
		if n < 1 || n > 2147483647 || numbers[n] {
			return invalid()
		}
		numbers[n] = true
	}
	channels := map[string]bool{}
	for _, s := range c.Channels {
		if !validChannel(s) || channels[s] {
			return invalid()
		}
		channels[s] = true
	}
	return nil
}

func (e *Engine) validateConfiguration(c Configuration) error {
	if err := validateConfigurationShape(c); err != nil {
		return err
	}
	capability, ok := e.capabilities[c.CapabilityID]
	if !ok {
		return invalid()
	}
	for _, channel := range c.Channels {
		if !slices.Contains(capability.Channels, channel) {
			return invalid()
		}
	}
	return nil
}

// Configure retains every configuration revision. An update never silently
// rebinds an already queued request to another provider or source policy.
func (e *Engine) Configure(ctx context.Context, scope core.Scope, key string, c Configuration) (json.RawMessage, error) {
	scope = scope.RequireAdministration()
	if err := validateConfigurationShape(c); err != nil {
		return nil, err
	}
	return e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.configure", Target: "configuration", Key: key, Request: c}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM review_execution_config WHERE project_id=?`, scope.ProjectID).Scan(&revision); err != nil {
			return nil, err
		}
		var previous Configuration
		if revision > 0 {
			var err error
			previous, _, err = readConfiguration(ctx, tx, scope.ProjectID)
			if err != nil {
				return nil, err
			}
		}
		operator, err := core.IsInstanceAdministrator(ctx, tx, scope.PrincipalID)
		if err != nil {
			return nil, err
		}
		if !operator && (revision == 0 || c.CapabilityID != previous.CapabilityID) {
			return nil, core.Fail("operator_capability_binding_required", "A service operator must establish or change this project's provider capability; preserve its current capability_id for project management.", 403)
		}
		if err := e.validateConfiguration(c); err != nil {
			return nil, err
		}
		if revision != *c.ExpectedRevision {
			return nil, conflict()
		}
		defaults, err := enrollmentRoundDefaults(ctx, tx, scope.ProjectID)
		if err != nil {
			return nil, err
		}
		for _, pull := range c.Queue {
			for _, channel := range c.Channels {
				if slices.Contains(previous.Queue, pull) && slices.Contains(previous.Channels, channel) {
					continue
				}
				// Removing and re-adding a legacy pair must not grant new rounds
				// over retained native work that was accepted without a control.
				var retained bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_operations WHERE project_id=? AND pull_number=? AND channel=?)`, scope.ProjectID, pull, channel).Scan(&retained); err != nil {
					return nil, err
				}
				if retained {
					continue
				}
				bounds := defaults.CLI
				if channel == "hosted" {
					bounds = defaults.Hosted
				}
				if err := initializeRoundControl(ctx, tx, scope, pull, channel, bounds.Minimum, bounds.Maximum); err != nil {
					return nil, err
				}
			}
		}
		revision++
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_execution_config(project_id,revision,body_json) VALUES(?,?,?)`, scope.ProjectID, revision, encoded(c)); err != nil {
			return nil, err
		}
		if err := core.AppendEvent(ctx, tx, scope, "reviews.configured", scope.ProjectID, revision, map[string]any{"revision": revision, "paused": c.Paused}); err != nil {
			return nil, err
		}
		return Receipt{ID: scope.ProjectID, State: "configured", Revision: revision}, nil
	})
}

func readConfiguration(ctx context.Context, tx *sql.Tx, project string) (Configuration, int64, error) {
	var rawRevision, rawBody any
	var c Configuration
	err := tx.QueryRowContext(ctx, `SELECT revision,body_json FROM review_execution_config WHERE project_id=? ORDER BY revision DESC LIMIT 1`, project).Scan(&rawRevision, &rawBody)
	if errors.Is(err, sql.ErrNoRows) {
		return c, 0, core.Fail("reviews_unconfigured", "Review execution is not configured for this project.", 409)
	}
	if err != nil {
		return c, 0, err
	}
	revision, revisionOK := rawRevision.(int64)
	body, bodyOK := rawBody.(string)
	if !revisionOK || revision < 1 || !bodyOK || json.Unmarshal([]byte(body), &c) != nil {
		return c, 0, automaticProjectStateFailure("storage_unavailable", "Stored review configuration is unavailable.", 500)
	}
	return c, revision, nil
}

// Enqueue returns a durable compact receipt without contacting any provider or
// preparing a workspace. Exact key replay survives later config/head movement.
func (e *Engine) Enqueue(ctx context.Context, scope core.Scope, key string, r Request) (json.RawMessage, error) {
	if r.ExpectedConfigurationRevision < 1 || r.PullNumber < 1 || r.PullNumber > 2147483647 || !validChannel(r.Channel) || !sha.MatchString(r.HeadSHA) {
		return nil, invalid()
	}
	result, err := e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.request", Target: "requests", Key: key, Request: r}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		return e.enqueueRequestTx(ctx, tx, scope, r)
	})
	if err == nil {
		select {
		case e.wake <- struct{}{}:
		default:
		}
	}
	return result, err
}

const (
	requestProjectCapacity = 32
	requestGlobalCapacity  = 256
)

// enqueueRequestTx preserves ordinary admission inside a selection child transaction.
func (e *Engine) enqueueRequestTx(ctx context.Context, tx *sql.Tx, scope core.Scope, r Request) (Receipt, error) {
	if err := requirePullScheduling(ctx, tx, scope.ProjectID, r.PullNumber); err != nil {
		return Receipt{}, err
	}
	c, revision, err := readConfiguration(ctx, tx, scope.ProjectID)
	if err != nil {
		return Receipt{}, err
	}
	if revision != r.ExpectedConfigurationRevision {
		return Receipt{}, conflict()
	}
	if c.Paused {
		return Receipt{}, core.Fail("reviews_paused", "Review execution is paused for this project.", 409)
	}
	if err = e.validateConfiguration(c); err != nil {
		return Receipt{}, core.Fail("capability_unavailable", "The configured review capability is unavailable.", 409)
	}
	if !slices.Contains(c.Queue, r.PullNumber) || !slices.Contains(c.Channels, r.Channel) {
		return Receipt{}, core.Fail("review_not_configured", "The selected PR or channel is not configured.", 409)
	}
	var pending string
	err = tx.QueryRowContext(ctx, `SELECT id FROM review_operations WHERE project_id=? AND pull_number=? AND channel=? AND state IN ('queued','preparing','posting','active','uncertain') ORDER BY created_at,id LIMIT 1`, scope.ProjectID, r.PullNumber, r.Channel).Scan(&pending)
	if err == nil {
		return Receipt{}, &core.Error{Code: "review_inflight", Message: "This PR already has a pending review request; keep or control that original operation.", Status: 409, Details: map[string]string{"operation_id": pending}}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, err
	}
	if err := requireSourceObligationsClear(ctx, tx, scope.ProjectID, r.PullNumber); err != nil {
		return Receipt{}, err
	}
	policy, err := readPolicy(ctx, tx, scope.ProjectID)
	if err != nil {
		return Receipt{}, err
	}
	if slices.Contains(policy.Policy.PausedChannels, r.Channel) {
		return Receipt{}, core.Fail("reviews_paused", "Review execution is paused for this channel.", 409)
	}
	var local, total int
	if err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(CASE WHEN project_id=? THEN 1 ELSE 0 END),0) FROM review_operations WHERE state IN ('queued','preparing','posting','active','uncertain')`, scope.ProjectID).Scan(&total, &local); err != nil {
		return Receipt{}, err
	}
	if local >= requestProjectCapacity || total >= requestGlobalCapacity {
		return Receipt{}, core.Fail("overloaded", "Review request capacity is occupied; recover or finish existing operations.", 503)
	}
	id, at := core.NewID("review_operation"), stamp()
	if _, err = tx.ExecContext(ctx, `INSERT INTO review_operations(id,project_id,actor_id,worker_id,configuration_revision,policy_revision,capability_fingerprint,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,'queued',1,'','',?,?)`, id, scope.ProjectID, scope.PrincipalID, scope.WorkerID, revision, policy.Revision, e.capabilities[c.CapabilityID].Fingerprint, r.PullNumber, r.Channel, r.HeadSHA, at, at); err != nil {
		return Receipt{}, err
	}
	if err = bindRoundControl(ctx, tx, Operation{ID: id, ProjectID: scope.ProjectID, PullNumber: r.PullNumber, Channel: r.Channel}); err != nil {
		return Receipt{}, err
	}
	if err = core.AppendEvent(ctx, tx, scope, "review.requested", id, 1, map[string]any{"pull_number": r.PullNumber, "channel": r.Channel}); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: id, State: "queued", Revision: 1}, nil
}

func readOperation(ctx context.Context, tx EvidenceQuery, project, id string) (Operation, error) {
	o, err := readOperationBase(ctx, tx, project, id)
	if err == nil {
		o.Allowance, err = readHeldAllowance(ctx, tx, o)
	}
	return o, err
}

func readOperationBase(ctx context.Context, tx EvidenceQuery, project, id string) (Operation, error) {
	var o Operation
	orderColumn, limitColumn, err := operationFlagColumns(ctx, tx)
	if err != nil {
		return o, err
	}
	err = tx.QueryRowContext(ctx, `SELECT id,project_id,actor_id,worker_id,configuration_revision,policy_revision,capability_fingerprint,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at,`+orderColumn+`,`+limitColumn+` FROM review_operations WHERE project_id=? AND id=?`, project, id).Scan(&o.ID, &o.ProjectID, &o.ActorID, &o.WorkerID, &o.ConfigurationRevision, &o.PolicyRevision, &o.CapabilityFingerprint, &o.PullNumber, &o.Channel, &o.HeadSHA, &o.State, &o.Revision, &o.Reason, &o.AttemptID, &o.CreatedAt, &o.UpdatedAt, &o.AllowOutOfOrder, &o.OverrideFileLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return o, core.Fail("not_found", "The review operation is unavailable in this project.", 404)
	}
	if err == nil {
		o.Wait, err = readOperationWait(ctx, tx, o)
	}
	return o, err
}

// Older retained snapshots supply false only for flags introduced after their
// declared version. Missing or malformed current columns fail the operation read.
func operationFlagColumns(ctx context.Context, q EvidenceQuery) (string, string, error) {
	var version string
	if err := q.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:review_execution'`).Scan(&version); err != nil {
		return "", "", err
	}
	const order = `CASE WHEN typeof(allow_out_of_order)='integer' AND allow_out_of_order IN (0,1) THEN allow_out_of_order END`
	const limit = `CASE WHEN typeof(override_file_limit)='integer' AND override_file_limit IN (0,1) THEN override_file_limit END`
	schemaVersion, err := strconv.Atoi(version)
	if err != nil || schemaVersion < 1 || strconv.Itoa(schemaVersion) != version {
		return "", "", adjudicationStorageError()
	}
	switch {
	case schemaVersion < 7:
		return "0", "0", nil
	case schemaVersion == 7:
		return order, "0", nil
	default:
		return order, limit, nil
	}
}

func (e *Engine) ReadOperation(ctx context.Context, scope core.Scope, id string) (Operation, error) {
	if !identifier.MatchString(id) {
		return Operation{}, invalid()
	}
	var o Operation
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		o, err = readOperation(ctx, tx, scope.ProjectID, id)
		o.Wait = currentOperationWait(o)
		return err
	})
	return o, err
}
