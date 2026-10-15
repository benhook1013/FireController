package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type SourceRefreshRequest struct {
	ExpectedConfigurationRevision int64  `json:"expected_configuration_revision"`
	PullNumber                    int64  `json:"pull_number"`
	HeadSHA                       string `json:"head_sha"`
}

// SourceObservation is a complete selected comparison as of ObservedAt, not a
// promise about current GitHub source, review sufficiency, CI or readiness.
type SourceObservation struct {
	RepositoryID     int64  `json:"repository_id"`
	RepositoryName   string `json:"repository_name"`
	PullNumber       int64  `json:"pull_number"`
	HeadSHA          string `json:"head_sha"`
	BaseSHA          string `json:"base_sha"`
	BaseRef          string `json:"base_ref"`
	ParentPullNumber int64  `json:"parent_pull_number"`
	ParentSHA        string `json:"parent_sha"`
	ObservedAt       string `json:"observed_at"`
}

type SourceRefreshOperation struct {
	ID                    string             `json:"id"`
	ProjectID             string             `json:"project_id"`
	PullNumber            int64              `json:"pull_number"`
	HeadSHA               string             `json:"head_sha"`
	ConfigurationRevision int64              `json:"configuration_revision"`
	State                 string             `json:"state"`
	Revision              int64              `json:"revision"`
	Reason                string             `json:"reason"`
	CreatedAt             string             `json:"created_at"`
	UpdatedAt             string             `json:"updated_at"`
	Observation           *SourceObservation `json:"observation,omitempty"`
}

type sourceRefresh struct {
	SourceRefreshOperation
	repositoryID                           int64
	actor, worker, capability, fingerprint string
}

func validRefreshRequest(r SourceRefreshRequest) bool {
	return r.ExpectedConfigurationRevision >= 1 && r.PullNumber >= 1 && r.PullNumber <= 2147483647 && sha.MatchString(r.HeadSHA)
}
func validRefreshID(id string) bool { return hostedObservationID(id, "review_refresh_") }
func refreshStorageError() error {
	return core.Fail("storage_unavailable", "Stored source refresh is unavailable.", 500)
}
func refreshMissing() error {
	return core.Fail("reviews_source_unavailable", "Explicitly refresh the selected source comparison before assessing it.", 409)
}

// EnqueueSourceRefresh returns a durable receipt without provider reads or spend.
// Only currently queued/running operations from the same actor and worker can
// coalesce. Exact key recovery always returns its original immutable receipt.
func (e *Engine) EnqueueSourceRefresh(ctx context.Context, scope core.Scope, key string, r SourceRefreshRequest) (json.RawMessage, error) {
	if ctx == nil || e == nil || e.store == nil || !validRefreshRequest(r) {
		return nil, invalid()
	}
	result, err := e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.source_refresh", Target: "source-refreshes", Key: key, Request: r}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		c, revision, err := readConfiguration(ctx, tx, scope.ProjectID)
		if err != nil {
			return nil, err
		}
		if revision != r.ExpectedConfigurationRevision {
			return nil, conflict()
		}
		if e.validateConfiguration(c) != nil {
			return nil, core.Fail("capability_unavailable", "The configured source reader is unavailable.", 409)
		}
		if !slices.Contains(c.Queue, r.PullNumber) {
			return nil, core.Fail("review_not_configured", "The selected PR is not configured.", 409)
		}
		var repositoryID int64
		if err = tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, scope.ProjectID).Scan(&repositoryID); err != nil {
			return nil, err
		}
		if repositoryID <= 0 {
			return nil, refreshStorageError()
		}
		cap := e.capabilities[c.CapabilityID]
		var id string
		err = tx.QueryRowContext(ctx, `SELECT id FROM review_source_refreshes WHERE project_id=? AND repository_id=? AND pull_number=? AND head_sha=? AND configuration_revision=? AND capability_fingerprint=? AND actor_id=? AND worker_id=? AND state IN ('queued','running') ORDER BY state LIMIT 1`, scope.ProjectID, repositoryID, r.PullNumber, r.HeadSHA, revision, cap.Fingerprint, scope.PrincipalID, scope.WorkerID).Scan(&id)
		if err == nil {
			return Receipt{ID: id, State: "queued", Revision: 1}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		var total, local int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM review_source_refreshes WHERE state IN ('queued','running') LIMIT 64)`).Scan(&total); err != nil {
			return nil, err
		}
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM review_source_refreshes WHERE project_id=? AND state IN ('queued','running') LIMIT 16)`, scope.ProjectID).Scan(&local); err != nil {
			return nil, err
		}
		if total >= 64 || local >= 16 {
			return nil, core.Fail("overloaded", "Source refresh capacity is occupied; wait for existing refreshes to finish.", 503)
		}
		id = core.NewID("review_refresh")
		at := sourceStamp()
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_source_refreshes(id,project_id,repository_id,actor_id,worker_id,configuration_revision,capability_id,capability_fingerprint,pull_number,head_sha,state,revision,reason,created_at,updated_at,observation_json) VALUES(?,?,?,?,?,?,?,?,?,?,'queued',1,'',?,?,'null')`, id, scope.ProjectID, repositoryID, scope.PrincipalID, scope.WorkerID, revision, cap.ID, cap.Fingerprint, r.PullNumber, r.HeadSHA, at, at); err != nil {
			return nil, err
		}
		if err = core.AppendEvent(ctx, tx, scope, "review.source_refresh_requested", id, 1, map[string]any{"pull_number": r.PullNumber}); err != nil {
			return nil, err
		}
		return Receipt{ID: id, State: "queued", Revision: 1}, nil
	})
	if err == nil {
		select {
		case e.sourceWake <- struct{}{}:
		default:
		}
	}
	return result, err
}

const refreshColumns = `id,project_id,repository_id,actor_id,worker_id,configuration_revision,capability_id,capability_fingerprint,pull_number,head_sha,state,revision,reason,created_at,updated_at,observation_json`

func readSourceRefresh(ctx context.Context, tx *sql.Tx, project, id string) (sourceRefresh, error) {
	var r sourceRefresh
	var body string
	err := tx.QueryRowContext(ctx, `SELECT `+refreshColumns+` FROM review_source_refreshes WHERE project_id=? AND id=?`, project, id).Scan(&r.ID, &r.ProjectID, &r.repositoryID, &r.actor, &r.worker, &r.ConfigurationRevision, &r.capability, &r.fingerprint, &r.PullNumber, &r.HeadSHA, &r.State, &r.Revision, &r.Reason, &r.CreatedAt, &r.UpdatedAt, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return r, core.Fail("not_found", "The source refresh is unavailable in this project.", 404)
	}
	if err != nil {
		return r, err
	}
	if !validRefreshID(r.ID) || r.ProjectID != project || !identifier.MatchString(project) || r.repositoryID <= 0 || !identifier.MatchString(r.actor) || r.worker != "" && !identifier.MatchString(r.worker) || !identifier.MatchString(r.capability) || !hash.MatchString(r.fingerprint) || !validRefreshRequest(SourceRefreshRequest{r.ConfigurationRevision, r.PullNumber, r.HeadSHA}) || r.Revision < 1 || !refreshTime(r.CreatedAt) || !refreshTime(r.UpdatedAt) || r.Reason != "" && !identifier.MatchString(r.Reason) || len(body) > 4096 {
		return r, refreshStorageError()
	}
	if r.State != "queued" && r.State != "running" && r.State != "completed" && r.State != "held" {
		return r, refreshStorageError()
	}
	if body != "null" && decodeSourceObservation(body, &r.Observation) != nil || (r.State == "completed") != (r.Observation != nil) {
		return r, refreshStorageError()
	}
	if r.Observation != nil && (!validSourceObservation(*r.Observation) || r.Observation.RepositoryID != r.repositoryID || r.Observation.PullNumber != r.PullNumber || r.Observation.HeadSHA != r.HeadSHA) {
		return r, refreshStorageError()
	}
	return r, nil
}
func decodeSourceObservation(body string, observation **SourceObservation) error {
	if err := strictjson.Decode([]byte(body), observation); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &fields) != nil {
		return refreshStorageError()
	}
	for _, key := range []string{"repository_id", "repository_name", "pull_number", "head_sha", "base_sha", "base_ref", "parent_pull_number", "parent_sha", "observed_at"} {
		raw, ok := fields[key]
		if !ok || string(raw) == "null" {
			return refreshStorageError()
		}
	}
	return nil
}
func sourceStamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z") }
func refreshTime(value string) bool {
	at, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !at.IsZero()
}
func validSourceObservation(o SourceObservation) bool {
	return o.RepositoryID > 0 && repository.MatchString(o.RepositoryName) && o.PullNumber >= 1 && o.PullNumber <= 2147483647 && sha.MatchString(o.HeadSHA) && sha.MatchString(o.BaseSHA) && safeText(o.BaseRef, 256) && o.BaseRef != "" && o.ParentPullNumber >= 0 && o.ParentPullNumber <= 2147483647 && o.ParentPullNumber != o.PullNumber && sha.MatchString(o.ParentSHA) && o.ParentSHA == o.BaseSHA && refreshTime(o.ObservedAt)
}
func (e *Engine) ReadSourceRefresh(ctx context.Context, scope core.Scope, id string) (SourceRefreshOperation, error) {
	if ctx == nil || e == nil || e.store == nil || !validRefreshID(id) {
		return SourceRefreshOperation{}, invalid()
	}
	var result SourceRefreshOperation
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		r, err := readSourceRefresh(ctx, tx, scope.ProjectID, id)
		if err == nil {
			result = r.SourceRefreshOperation
		}
		return err
	})
	if err != nil {
		return SourceRefreshOperation{}, err
	}
	return result, nil
}

// refreshBinding checks current configuration and authority without treating
// execution pauses or channel policy as permission to forbid a source read.
func (e *Engine) refreshBinding(ctx context.Context, tx *sql.Tx, r sourceRefresh) (Configuration, string, error) {
	c, _, err := readConfiguration(ctx, tx, r.ProjectID)
	if err != nil {
		var failure *core.Error
		if errors.As(err, &failure) && failure.Code == "reviews_unconfigured" {
			return c, "source_configuration_changed", nil
		}
		return c, "", err
	}
	var repo int64
	if err = tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, r.ProjectID).Scan(&repo); err != nil {
		return c, "", err
	}
	cap, ok := e.capabilities[c.CapabilityID]
	retained, err := sourceConfiguration(ctx, tx, r.ProjectID, r.ConfigurationRevision)
	if err != nil {
		return c, "", err
	}
	if !sameSourceConfiguration(c, retained) || repo != r.repositoryID || !ok || e.validateConfiguration(c) != nil || c.CapabilityID != r.capability || cap.Fingerprint != r.fingerprint || !slices.Contains(c.Queue, r.PullNumber) {
		return c, "source_configuration_changed", nil
	}
	allowed, err := actorAllowed(ctx, tx, r.ProjectID, r.actor, r.worker)
	if err != nil {
		return c, "", err
	}
	if !allowed {
		return c, "authorization_changed", nil
	}
	return c, "", nil
}
func sameSourceConfiguration(a, b Configuration) bool {
	return a.CapabilityID == b.CapabilityID && a.Mode == b.Mode && slices.Equal(a.Queue, b.Queue) && slices.Equal(a.Channels, b.Channels)
}
func sourceConfiguration(ctx context.Context, tx *sql.Tx, project string, revision int64) (Configuration, error) {
	var c Configuration
	var body string
	if err := tx.QueryRowContext(ctx, `SELECT body_json FROM review_execution_config WHERE project_id=? AND revision=?`, project, revision).Scan(&body); err != nil {
		return c, err
	}
	if len(body) > 8192 || strictjson.Decode([]byte(body), &c) != nil || validateConfigurationShape(c) != nil {
		return c, refreshStorageError()
	}
	return c, nil
}
func (e *Engine) updateSourceRefresh(ctx context.Context, tx *sql.Tx, r sourceRefresh, state, reason string, observation *SourceObservation) error {
	result, err := tx.ExecContext(ctx, `UPDATE review_source_refreshes SET state=?,reason=?,revision=revision+1,updated_at=?,observation_json=? WHERE id=? AND project_id=? AND revision=? AND state=?`, state, reason, sourceStamp(), encoded(observation), r.ID, r.ProjectID, r.Revision, r.State)
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
	return core.AppendEvent(ctx, tx, core.Scope{ProjectID: r.ProjectID, PrincipalID: r.actor, WorkerID: r.worker}, "review.source_refresh_updated", r.ID, r.Revision+1, map[string]any{"state": state, "reason": reason})
}

// StartSourceRefresh owns one source-only worker. Interrupted running reads are
// requeued synchronously; no external action can be repeated by that recovery.
func (e *Engine) StartSourceRefresh(ctx context.Context, readers map[string]TargetReader) (<-chan error, error) {
	if ctx == nil || e == nil || e.store == nil {
		return nil, invalid()
	}
	if len(readers) == 0 {
		return nil, nil
	}
	owned := make(map[string]TargetReader, len(readers))
	for id, r := range readers {
		if _, ok := e.capabilities[id]; !ok || r == nil || reflect.ValueOf(r).Kind() == reflect.Pointer && reflect.ValueOf(r).IsNil() {
			return nil, invalid()
		}
		owned[id] = r
	}
	if !e.sourceRunning.CompareAndSwap(false, true) {
		return nil, conflict()
	}
	var recovered []string
	if err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// Active work is bounded globally at 64. Collect IDs before updating rows.
		rows, err := tx.QueryContext(ctx, `SELECT project_id,id FROM review_source_refreshes WHERE state='running' ORDER BY created_at,id LIMIT 65`)
		if err != nil {
			return err
		}
		type id struct{ project, value string }
		var ids []id
		for rows.Next() {
			var next id
			if err = rows.Scan(&next.project, &next.value); err != nil {
				break
			}
			ids = append(ids, next)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		if len(ids) > 64 {
			return refreshStorageError()
		}
		for _, next := range ids {
			r, err := readSourceRefresh(ctx, tx, next.project, next.value)
			if err != nil {
				return err
			}
			if err = e.updateSourceRefresh(ctx, tx, r, "queued", "source_read_interrupted", nil); err != nil {
				return err
			}
			recovered = append(recovered, r.ID)
		}
		return nil
	}); err != nil {
		e.sourceRunning.Store(false)
		return nil, err
	}
	for _, id := range recovered {
		e.notifySourceRefresh(id)
	}
	done := make(chan error, 1)
	go func() { err := e.sourceRefreshLoop(ctx, owned); e.sourceRunning.Store(false); done <- err; close(done) }()
	return done, nil
}
func (e *Engine) sourceRefreshLoop(ctx context.Context, readers map[string]TargetReader) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-e.sourceWake:
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return nil
		}
		worked, err := e.refreshNext(ctx, readers)
		if err != nil {
			var failure *core.Error
			cancelled := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &failure) && failure.Code == "overloaded"
			if ctx.Err() != nil && cancelled {
				return nil
			}
			return core.Fail("source_refresh_persistence_failed", "Source refresh stopped because local state could not be recorded; restart to recover pending reads.", 503)
		}
		if worked {
			timer.Reset(0)
		} else {
			timer.Reset(500 * time.Millisecond)
		}
	}
}
func (e *Engine) refreshNext(ctx context.Context, readers map[string]TargetReader) (bool, error) {
	var selected *sourceRefresh
	var configuration Configuration
	var changed string

	var project, id string
	var expected int64
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT project_id,id,revision FROM review_source_refreshes WHERE state='queued' ORDER BY created_at,id LIMIT 1`).Scan(&project, &id, &expected)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil || id == "" {
		return false, err
	}
	err = e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		r, err := readSourceRefresh(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if r.State != "queued" || r.Revision != expected {
			return nil
		}
		var reason string
		configuration, reason, err = e.refreshBinding(ctx, tx, r)
		if err != nil {
			return err
		}
		if readers[r.capability] == nil && reason == "" {
			reason = "source_reader_unavailable"
		}
		if reason != "" {
			changed = r.ID
			return e.updateSourceRefresh(ctx, tx, r, "held", reason, nil)
		}
		if err = e.updateSourceRefresh(ctx, tx, r, "running", "", nil); err != nil {
			return err
		}
		changed = r.ID
		r.State = "running"
		r.Revision++
		selected = &r
		return nil
	})
	if err == nil && changed != "" {
		e.notifySourceRefresh(changed)
	}
	if err != nil || selected == nil {
		return changed != "", err
	}
	r := *selected
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	plan, readErr := ResolveSourceObservation(readCtx, readers[r.capability], r.repositoryID, configuration, r.PullNumber, r.HeadSHA)
	cancel()
	if ctx.Err() != nil {
		return true, nil
	}
	err = e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := readSourceRefresh(ctx, tx, r.ProjectID, r.ID)
		if err != nil {
			return err
		}
		if current.State != r.State || current.Revision != r.Revision {
			return conflict()
		}
		_, reason, err := e.refreshBinding(ctx, tx, r)
		if err != nil {
			return err
		}
		if reason == "" && readErr != nil {
			reason = sourceRefreshReason(readErr)
		}
		if reason != "" {
			return e.updateSourceRefresh(ctx, tx, r, "held", reason, nil)
		}
		observation := SourceObservation{RepositoryID: plan.Repository.ID, RepositoryName: plan.Repository.FullName, PullNumber: plan.Pull.Number, HeadSHA: plan.Pull.HeadSHA, BaseSHA: plan.Pull.BaseSHA, BaseRef: plan.Pull.BaseRef, ParentPullNumber: plan.ParentPullNumber, ParentSHA: plan.ParentSHA, ObservedAt: plan.ObservedAt}
		if !validSourceObservation(observation) || observation.RepositoryID != r.repositoryID || observation.PullNumber != r.PullNumber || observation.HeadSHA != r.HeadSHA {
			return e.updateSourceRefresh(ctx, tx, r, "held", "source_unavailable", nil)
		}
		return e.updateSourceRefresh(ctx, tx, r, "completed", "", &observation)
	})
	if err == nil {
		e.notifySourceRefresh(r.ID)
	}
	return true, err
}
func sourceRefreshReason(err error) string {
	var failure *core.Error
	if errors.As(err, &failure) && failure.Code == "review_held" {
		if details, ok := failure.Details.(map[string]string); ok {
			switch reason := details["reason"]; reason {
			case "repository_changed", "target_changed", "parent_identity_changed", "parent_unavailable", "default_base_changed", "parent_moved", "observation_incomplete":
				return reason
			}
		}
	}
	return "source_unavailable"
}

type SourcePolicyAssessment struct {
	ProjectID             string                    `json:"project_id"`
	RefreshID             string                    `json:"refresh_id"`
	Source                string                    `json:"source"`
	ConfigurationRevision int64                     `json:"configuration_revision"`
	PolicyRevision        int64                     `json:"policy_revision"`
	SourceObservedAt      string                    `json:"source_observed_at"`
	EvaluatedAt           string                    `json:"evaluated_at"`
	State                 string                    `json:"state"`
	Reason                string                    `json:"reason,omitempty"`
	Target                SourceObservation         `json:"target"`
	Channels              []PolicyAssessmentChannel `json:"channels"`
}

func (e *Engine) ReadSourcePolicyAssessment(ctx context.Context, scope core.Scope, number int64) (SourcePolicyAssessment, error) {
	if ctx == nil || e == nil || e.store == nil || number < 1 || number > 2147483647 {
		return SourcePolicyAssessment{}, invalid()
	}
	var result SourcePolicyAssessment
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM review_source_refreshes WHERE project_id=? AND pull_number=? AND state='completed' ORDER BY created_at DESC,id DESC LIMIT 1`, scope.ProjectID, number).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return refreshMissing()
		}
		if err != nil {
			return err
		}
		r, err := readSourceRefresh(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return err
		}
		observation := *r.Observation
		result = SourcePolicyAssessment{ProjectID: scope.ProjectID, RefreshID: r.ID, Source: "observed_comparison", ConfigurationRevision: r.ConfigurationRevision, SourceObservedAt: observation.ObservedAt, EvaluatedAt: stamp(), State: "unconfigured", Target: observation, Channels: []PolicyAssessmentChannel{}}
		// Read access is current, not inherited from the actor who refreshed. Only
		// repository/configuration/capability identity determines source applicability.
		c, revision, err := readConfiguration(ctx, tx, scope.ProjectID)
		var currentRepository int64
		if err == nil {
			err = tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, scope.ProjectID).Scan(&currentRepository)
		}
		if err != nil {
			var failure *core.Error
			if !errors.As(err, &failure) || failure.Code != "reviews_unconfigured" {
				return err
			}
		}
		cap, ok := e.capabilities[c.CapabilityID]
		var retained Configuration
		if err == nil {
			retained, err = sourceConfiguration(ctx, tx, r.ProjectID, r.ConfigurationRevision)
			if err != nil {
				return err
			}
		}
		result.ConfigurationRevision = revision
		stale := err != nil || !sameSourceConfiguration(c, retained) || currentRepository != r.repositoryID || !ok || e.validateConfiguration(c) != nil || c.CapabilityID != r.capability || cap.Fingerprint != r.fingerprint || !slices.Contains(c.Queue, number)
		policy, err := readPolicy(ctx, tx, scope.ProjectID)
		if err != nil {
			var failure *core.Error
			if !errors.As(err, &failure) || failure.Code != "reviews_policy_unconfigured" {
				return err
			}
			if stale {
				result.State = "held"
				result.Reason = "source_configuration_changed"
			}
			return nil
		}
		result.PolicyRevision = policy.Revision
		if stale {
			result.State = "held"
			result.Reason = "source_configuration_changed"
			return nil
		}
		result.State = "sufficient"
		anchor := Anchor{RepositoryID: observation.RepositoryID, Repository: observation.RepositoryName, PullNumber: number, CandidateSHA: observation.HeadSHA, BaseSHA: observation.BaseSHA, BaseRef: observation.BaseRef, ParentPullNumber: observation.ParentPullNumber, ParentSHA: observation.ParentSHA, ObservedAt: observation.ObservedAt}
		for _, channel := range policy.Policy.RequiredChannels {
			state, err := comparison(ctx, tx, scope.ProjectID, r.repositoryID, number, anchor, channel)
			if err != nil {
				return err
			}
			result.Channels = append(result.Channels, PolicyAssessmentChannel{Channel: channel, Covered: state.Covered, Pending: state.Pending})
			if !state.Covered || state.Pending {
				result.State = "held"
			}
		}
		result.EvaluatedAt = stamp()
		return nil
	})
	if err != nil {
		return SourcePolicyAssessment{}, err
	}
	return result, nil
}

// Each waiter owns its notification channel and original read scope. There is
// no SQL/provider polling: only committed state notifications cause rereads.
type sourceRefreshWaiter struct {
	principal string
	changed   chan struct{}
}

func (e *Engine) notifySourceRefresh(id string) {
	e.sourceMu.Lock()
	defer e.sourceMu.Unlock()
	for waiter := range e.sourceWaiters[id] {
		select {
		case waiter.changed <- struct{}{}:
		default:
		}
	}
}
func (e *Engine) WaitSourceRefresh(ctx context.Context, scope core.Scope, id string, maxWait time.Duration) (SourceRefreshOperation, error) {
	if ctx == nil || e == nil || e.store == nil || !validRefreshID(id) || !identifier.MatchString(scope.PrincipalID) || maxWait < 0 || maxWait > 60*time.Second {
		return SourceRefreshOperation{}, invalid()
	}
	if maxWait == 0 {
		return e.ReadSourceRefresh(ctx, scope, id)
	}
	waiter := &sourceRefreshWaiter{principal: scope.PrincipalID, changed: make(chan struct{}, 1)}
	e.sourceMu.Lock()
	if e.sourceWaiterCount >= 256 || e.sourceWaiterPrincipals[waiter.principal] >= 8 {
		e.sourceMu.Unlock()
		return SourceRefreshOperation{}, &core.Error{Code: "overloaded", Message: "Too many source refresh waits are open; read the retained operation explicitly.", Status: 503, Details: map[string]string{"reason": "source_wait_capacity"}}
	}
	if e.sourceWaiters[id] == nil {
		e.sourceWaiters[id] = make(map[*sourceRefreshWaiter]struct{})
	}
	e.sourceWaiters[id][waiter] = struct{}{}
	e.sourceWaiterCount++
	e.sourceWaiterPrincipals[waiter.principal]++
	e.sourceMu.Unlock()
	defer func() {
		e.sourceMu.Lock()
		defer e.sourceMu.Unlock()
		delete(e.sourceWaiters[id], waiter)
		if len(e.sourceWaiters[id]) == 0 {
			delete(e.sourceWaiters, id)
		}
		e.sourceWaiterCount--
		e.sourceWaiterPrincipals[waiter.principal]--
		if e.sourceWaiterPrincipals[waiter.principal] == 0 {
			delete(e.sourceWaiterPrincipals, waiter.principal)
		}
	}()
	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	for {
		value, err := e.ReadSourceRefresh(ctx, scope, id)
		if err != nil {
			return SourceRefreshOperation{}, err
		}
		if value.State == "completed" || value.State == "held" {
			return value, nil
		}
		select {
		case <-ctx.Done():
			return SourceRefreshOperation{}, core.Fail("overloaded", "The source refresh wait was canceled; its retained operation is unchanged.", 503)
		case <-timer.C:
			return e.ReadSourceRefresh(ctx, scope, id)
		case <-waiter.changed:
		}
	}
}
