package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"slices"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
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
	if err := validateConfigurationShape(c); err != nil {
		return nil, err
	}
	return e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.configure", Target: "configuration", Key: key, Request: c}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		// Recheck admin at the transaction boundary even for direct Go callers.
		var admin int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM credentials WHERE principal_id=? AND is_admin=1 AND revoked=0`, scope.PrincipalID).Scan(&admin); err != nil {
			return nil, err
		}
		if admin != 1 {
			return nil, core.Fail("forbidden", "Administrative permission is required.", 403)
		}
		if err := e.validateConfiguration(c); err != nil {
			return nil, err
		}
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM review_execution_config WHERE project_id=?`, scope.ProjectID).Scan(&revision); err != nil {
			return nil, err
		}
		if revision != *c.ExpectedRevision {
			return nil, conflict()
		}
		var previous Configuration
		if revision > 0 {
			var err error
			previous, _, err = readConfiguration(ctx, tx, scope.ProjectID)
			if err != nil {
				return nil, err
			}
		}
		return e.persistConfiguration(ctx, tx, scope, previous, c, revision)
	})
}

// persistConfiguration is shared by administrative replacement and narrow
// ordinary queue edits. Callers establish authority and the exact prior revision.
func (e *Engine) persistConfiguration(ctx context.Context, tx *sql.Tx, scope core.Scope, previous, c Configuration, revision int64) (Receipt, error) {
	if revision == math.MaxInt64 {
		return Receipt{}, core.Fail("revision_exhausted", "The execution configuration revision cannot advance further.", 409)
	}
	// Enrolling a new PR/channel installs the product plan atomically.
	// Existing pairs are deliberately untouched by pause/order/capability
	// edits, including installations upgraded from unconfigured controls.
	for _, pull := range c.Queue {
		for _, channel := range c.Channels {
			if slices.Contains(previous.Queue, pull) && slices.Contains(previous.Channels, channel) {
				continue
			}
			maximum := int64(6)
			if channel == "hosted" {
				maximum = 3
			}
			if err := initializeRoundControl(ctx, tx, scope, pull, channel, 2, maximum); err != nil {
				return Receipt{}, err
			}
		}
	}
	if e.publishQueue != nil {
		if err := e.publishQueue(ctx, tx, scope, previous.Queue, c.Queue); err != nil {
			return Receipt{}, err
		}
	}
	revision++
	if _, err := tx.ExecContext(ctx, `INSERT INTO review_execution_config(project_id,revision,body_json) VALUES(?,?,?)`, scope.ProjectID, revision, encoded(c)); err != nil {
		return Receipt{}, err
	}
	if err := core.AppendEvent(ctx, tx, scope, "reviews.configured", scope.ProjectID, revision, map[string]any{"revision": revision, "paused": c.Paused}); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: scope.ProjectID, State: "configured", Revision: revision}, nil
}

func readConfiguration(ctx context.Context, tx *sql.Tx, project string) (Configuration, int64, error) {
	var body string
	var revision int64
	var c Configuration
	err := tx.QueryRowContext(ctx, `SELECT revision,body_json FROM review_execution_config WHERE project_id=? ORDER BY revision DESC LIMIT 1`, project).Scan(&revision, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return c, 0, core.Fail("reviews_unconfigured", "Review execution is not configured for this project.", 409)
	}
	if err != nil {
		return c, 0, err
	}
	if json.Unmarshal([]byte(body), &c) != nil {
		return c, 0, core.Fail("storage_unavailable", "Stored review configuration is unavailable.", 500)
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

// enqueueRequestTx shares native acceptance after the caller has established
// current authority. scope supplies checked attribution; it is not authentication.
func (e *Engine) enqueueRequestTx(ctx context.Context, tx *sql.Tx, scope core.Scope, r Request) (Receipt, error) {
	c, revision, err := readConfiguration(ctx, tx, scope.ProjectID)
	if err != nil {
		return Receipt{}, err
	}
	if revision != r.ExpectedConfigurationRevision {
		return Receipt{}, configurationRevisionConflict(revision)
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
	if err := requireSourceObligationsClear(ctx, tx, scope.ProjectID, r.PullNumber); err != nil {
		return Receipt{}, err
	}
	if err := requireIncomingRoutesClear(ctx, tx, scope.ProjectID, r.PullNumber); err != nil {
		return Receipt{}, err
	}
	policy, err := readPolicy(ctx, tx, scope.ProjectID)
	if err != nil {
		return Receipt{}, err
	}
	var local, total int
	if err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(CASE WHEN project_id=? THEN 1 ELSE 0 END),0) FROM review_operations WHERE state IN ('queued','preparing','posting','active','uncertain')`, scope.ProjectID).Scan(&total, &local); err != nil {
		return Receipt{}, err
	}
	if local >= 32 || total >= 256 {
		return Receipt{}, core.Fail("overloaded", "Review request capacity is occupied; recover or finish existing operations.", 503)
	}
	id, at := core.NewID("review_operation"), stamp()
	if _, err = tx.ExecContext(ctx, `INSERT INTO review_operations(id,project_id,actor_id,worker_id,configuration_revision,policy_revision,capability_fingerprint,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at,allow_out_of_order,override_file_limit) VALUES(?,?,?,?,?,?,?,?,?,?,'queued',1,'','',?,?,?,?)`, id, scope.ProjectID, scope.PrincipalID, scope.WorkerID, revision, policy.Revision, e.capabilities[c.CapabilityID].Fingerprint, r.PullNumber, r.Channel, r.HeadSHA, at, at, r.AllowOutOfOrder, r.OverrideFileLimit); err != nil {
		return Receipt{}, err
	}
	if err = bindRoundControl(ctx, tx, Operation{ID: id, ProjectID: scope.ProjectID, PullNumber: r.PullNumber, Channel: r.Channel, State: "queued"}); err != nil {
		return Receipt{}, err
	}
	if err = core.AppendEvent(ctx, tx, scope, "review.requested", id, 1, map[string]any{"pull_number": r.PullNumber, "channel": r.Channel}); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: id, State: "queued", Revision: 1, ReviewSize: reviews.ReviewSizeForHead(ctx, tx, scope.ProjectID, r.PullNumber, r.HeadSHA)}, nil
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
	switch version {
	case "1", "2", "3", "4", "5", "6":
		return "0", "0", nil
	case "7":
		return order, "0", nil
	case "8":
		return order, limit, nil
	default:
		return "", "", adjudicationStorageError()
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
		return err
	})
	return o, err
}
