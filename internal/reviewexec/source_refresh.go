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
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type SourceRefreshRequest struct {
	ExpectedConfigurationRevision int64  `json:"expected_configuration_revision"`
	PullNumber                    int64  `json:"pull_number"`
	HeadSHA                       string `json:"head_sha"`
	IncludeCI                     bool   `json:"include_ci,omitempty"`
	IncludeReadiness              bool   `json:"include_readiness,omitempty"`
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
	ID                    string                    `json:"id"`
	ProjectID             string                    `json:"project_id"`
	PullNumber            int64                     `json:"pull_number"`
	HeadSHA               string                    `json:"head_sha"`
	ConfigurationRevision int64                     `json:"configuration_revision"`
	State                 string                    `json:"state"`
	Revision              int64                     `json:"revision"`
	Reason                string                    `json:"reason"`
	CreatedAt             string                    `json:"created_at"`
	UpdatedAt             string                    `json:"updated_at"`
	Observation           *SourceObservation        `json:"observation,omitempty"`
	IncludeCI             bool                      `json:"include_ci,omitempty"`
	CI                    *SourceCISummary          `json:"ci,omitempty"`
	IncludeReadiness      bool                      `json:"include_readiness,omitempty"`
	ReviewState           *SourceReviewStateSummary `json:"review_state,omitempty"`
}

type sourceRefresh struct {
	SourceRefreshOperation
	repositoryID                           int64
	actor, worker, capability, fingerprint string
	ciObservation                          *reviewprovider.CIObservation
	reviewObservation                      *reviewprovider.PullReviewObservation
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
			return nil, configurationRevisionConflict(revision)
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
		err = tx.QueryRowContext(ctx, `SELECT id FROM review_source_refreshes WHERE project_id=? AND repository_id=? AND pull_number=? AND head_sha=? AND configuration_revision=? AND capability_fingerprint=? AND actor_id=? AND worker_id=? AND include_ci=? AND include_readiness=? AND state IN ('queued','running') ORDER BY state LIMIT 1`, scope.ProjectID, repositoryID, r.PullNumber, r.HeadSHA, revision, cap.Fingerprint, scope.PrincipalID, scope.WorkerID, r.IncludeCI, r.IncludeReadiness).Scan(&id)
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
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_source_refreshes(id,project_id,repository_id,actor_id,worker_id,configuration_revision,capability_id,capability_fingerprint,pull_number,head_sha,state,revision,reason,created_at,updated_at,observation_json,include_ci,include_readiness) VALUES(?,?,?,?,?,?,?,?,?,?,'queued',1,'',?,?,'null',?,?)`, id, scope.ProjectID, repositoryID, scope.PrincipalID, scope.WorkerID, revision, cap.ID, cap.Fingerprint, r.PullNumber, r.HeadSHA, at, at, r.IncludeCI, r.IncludeReadiness); err != nil {
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

const refreshMetadataColumns = `id,project_id,repository_id,actor_id,worker_id,configuration_revision,capability_id,capability_fingerprint,pull_number,head_sha,state,revision,reason,created_at,updated_at,observation_json,include_ci,include_readiness`

// readSourceRefreshMetadata reads only source and action identity. Policy and
// comparison-only reads never select or decode the private CI inventory.
func readSourceRefreshMetadata(ctx context.Context, tx *sql.Tx, project, id string) (sourceRefresh, error) {
	var r sourceRefresh
	var body string
	var includeCI, includeReadiness int64
	err := tx.QueryRowContext(ctx, `SELECT `+refreshMetadataColumns+` FROM review_source_refreshes WHERE project_id=? AND id=?`, project, id).Scan(&r.ID, &r.ProjectID, &r.repositoryID, &r.actor, &r.worker, &r.ConfigurationRevision, &r.capability, &r.fingerprint, &r.PullNumber, &r.HeadSHA, &r.State, &r.Revision, &r.Reason, &r.CreatedAt, &r.UpdatedAt, &body, &includeCI, &includeReadiness)
	if errors.Is(err, sql.ErrNoRows) {
		return r, core.Fail("not_found", "The source refresh is unavailable in this project.", 404)
	}
	if err != nil {
		return r, err
	}
	if !validRefreshID(r.ID) || r.ProjectID != project || !identifier.MatchString(project) || r.repositoryID <= 0 || !identifier.MatchString(r.actor) || r.worker != "" && !identifier.MatchString(r.worker) || !identifier.MatchString(r.capability) || !hash.MatchString(r.fingerprint) || !validRefreshRequest(SourceRefreshRequest{ExpectedConfigurationRevision: r.ConfigurationRevision, PullNumber: r.PullNumber, HeadSHA: r.HeadSHA}) || r.Revision < 1 || !refreshTime(r.CreatedAt) || !refreshTime(r.UpdatedAt) || r.Reason != "" && !identifier.MatchString(r.Reason) || len(body) > 4096 {
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
	if includeCI != 0 && includeCI != 1 || includeReadiness != 0 && includeReadiness != 1 {
		return r, refreshStorageError()
	}
	r.IncludeCI = includeCI == 1
	r.IncludeReadiness = includeReadiness == 1
	return r, nil
}

// Full operation and selected-inventory consumers keep strict validation inside
// the caller's same authorized snapshot, including background recovery reads.
func readSourceRefresh(ctx context.Context, tx *sql.Tx, project, id string) (sourceRefresh, error) {
	r, err := readSourceRefreshMetadata(ctx, tx, project, id)
	if err != nil {
		return r, err
	}
	var ciBody, reviewBody string
	if err = tx.QueryRowContext(ctx, `SELECT ci_observation_json,review_state_json FROM review_source_refreshes WHERE project_id=? AND id=?`, project, id).Scan(&ciBody, &reviewBody); err != nil {
		return r, err
	}
	if err := decodeSourceCI(ciBody, &r); err != nil {
		return r, err
	}
	if err := decodeSourceReviewState(reviewBody, &r); err != nil {
		return r, err
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
	parent := int64(0)
	if r.Observation != nil {
		parent = r.Observation.ParentPullNumber
	}
	if !sameSourceTargetConfiguration(c, retained, r.PullNumber, parent) || repo != r.repositoryID || !ok || e.validateConfiguration(c) != nil || c.CapabilityID != r.capability || cap.Fingerprint != r.fingerprint || !slices.Contains(c.Queue, r.PullNumber) {
		return c, "source_configuration_changed", nil
	}
	allowed, err := actorAllowed(ctx, tx, r.ProjectID, r.actor, r.worker)
	if err != nil {
		return c, "", err
	}
	if !allowed {
		return c, "authorization_changed", nil
	}
	// The observed request keeps its original source-resolution plan.
	return retained, "", nil
}

// Source applicability depends on this target's prefix and actual parent
// membership, not unrelated later queue entries. Pause never forbids source
// reads. The actual parent can lie after the target or outside the queue; its
// identity comes from provider facts, while its enrollment must remain stable.
func sameSourceTargetConfiguration(a, b Configuration, number, parent int64) bool {
	if a.CapabilityID != b.CapabilityID || a.Mode != b.Mode || !slices.Equal(a.Channels, b.Channels) {
		return false
	}
	left, right := slices.Index(a.Queue, number), slices.Index(b.Queue, number)
	return left >= 0 && right == left && slices.Equal(a.Queue[:left+1], b.Queue[:right+1]) && (parent == 0 || slices.Contains(a.Queue, parent) == slices.Contains(b.Queue, parent))
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
func (e *Engine) updateSourceRefresh(ctx context.Context, tx *sql.Tx, r sourceRefresh, state, reason string, observation *SourceObservation, ciBody, reviewBody string) error {
	result, err := tx.ExecContext(ctx, `UPDATE review_source_refreshes SET state=?,reason=?,revision=revision+1,updated_at=?,observation_json=?,ci_observation_json=?,review_state_json=? WHERE id=? AND project_id=? AND revision=? AND state=?`, state, reason, sourceStamp(), encoded(observation), ciBody, reviewBody, r.ID, r.ProjectID, r.Revision, r.State)
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

const sourceRefreshWorkers = 4

const sourceRefreshSelection = `SELECT q.project_id,q.id,q.revision FROM review_source_refreshes q
 WHERE q.state='queued' AND NOT EXISTS (
 SELECT 1 FROM review_source_refreshes busy WHERE busy.project_id=q.project_id AND busy.state IN ('queued','running') AND busy.state='running')
 ORDER BY (q.project_id<=?),q.project_id,q.created_at,q.id LIMIT 1`

const sourceRefreshProjectBusy = `SELECT EXISTS(SELECT 1 FROM review_source_refreshes WHERE project_id=? AND state IN ('queued','running') AND state='running')`

// One Start owns this cursor and cancellable guard. Only selection/claim SQL is
// serialized; provider reads and guarded completion retain independent slots.
type sourceRefreshSchedule struct {
	claim       chan struct{}
	lastProject string
}

func newSourceRefreshSchedule() *sourceRefreshSchedule {
	return &sourceRefreshSchedule{claim: make(chan struct{}, 1)}
}

// StartSourceRefresh owns a bounded source-only pool. Interrupted running reads are
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
			if err = e.updateSourceRefresh(ctx, tx, r, "queued", "source_read_interrupted", nil, "null", "null"); err != nil {
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
	go func() {
		workerCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		schedule := newSourceRefreshSchedule()
		results := make(chan error, sourceRefreshWorkers)
		for range sourceRefreshWorkers {
			go func() { results <- e.sourceRefreshLoopScheduled(workerCtx, owned, schedule) }()
		}
		var first error
		for range sourceRefreshWorkers {
			if err := <-results; err != nil && first == nil {
				first = err
				cancel()
			}
		}
		e.sourceRunning.Store(false)
		done <- first
		close(done)
	}()
	return done, nil
}

// retrySourceRefreshLocal waits only for transient local admission contention.
// Callers retain any completed provider read outside this closure; retries must
// not repeat external work. Cancellation leaves startup recovery authoritative.
func retrySourceRefreshLocal(ctx context.Context, local func() error) error {
	delay := 100 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := local()
		var failure *core.Error
		if !errors.As(err, &failure) || failure.Code != "overloaded" {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, time.Second)
	}
}

func (e *Engine) sourceRefreshLoop(ctx context.Context, readers map[string]TargetReader) error {
	return e.sourceRefreshLoopScheduled(ctx, readers, newSourceRefreshSchedule())
}

func (e *Engine) sourceRefreshLoopScheduled(ctx context.Context, readers map[string]TargetReader, schedule *sourceRefreshSchedule) error {
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
		var worked bool
		err := retrySourceRefreshLocal(ctx, func() error {
			var err error
			worked, err = e.refreshNextScheduled(ctx, readers, schedule)
			return err
		})
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
	return e.refreshNextScheduled(ctx, readers, newSourceRefreshSchedule())
}

func (e *Engine) refreshNextScheduled(ctx context.Context, readers map[string]TargetReader, schedule *sourceRefreshSchedule) (bool, error) {
	var selected *sourceRefresh
	var configuration Configuration
	var metadataPins map[int64]reviews.SourceMetadataPin
	var changed string

	var project, id string
	var expected int64
	err := func() error {
		select {
		case schedule.claim <- struct{}{}:
			defer func() { <-schedule.claim }()
		case <-ctx.Done():
			return ctx.Err()
		}
		// Cycle eligible projects, then take their oldest queued row.
		err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
			err := tx.QueryRowContext(ctx, sourceRefreshSelection, schedule.lastProject).Scan(&project, &id, &expected)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		})
		if err != nil || id == "" {
			return err
		}
		err = e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			r, err := readSourceRefresh(ctx, tx, project, id)
			if err != nil {
				return err
			}
			if r.State != "queued" || r.Revision != expected {
				return nil
			}
			var busy bool
			// r is queued in this transaction, so any running row is another read.
			if err = tx.QueryRowContext(ctx, sourceRefreshProjectBusy, project).Scan(&busy); err != nil || busy {
				return err
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
				return e.updateSourceRefresh(ctx, tx, r, "held", reason, nil, "null", "null")
			}
			if e.publishAttempt != nil {
				metadataPins, err = reviews.PinSourceMetadata(ctx, tx, r.ProjectID, []int64{r.PullNumber})
				if err != nil {
					return err
				}
			}
			if err = e.updateSourceRefresh(ctx, tx, r, "running", "", nil, "null", "null"); err != nil {
				return err
			}
			changed = r.ID
			r.State = "running"
			r.Revision++
			selected = &r
			return nil
		})
		if err == nil && changed != "" {
			schedule.lastProject = project
		}
		return err
	}()
	if err == nil && changed != "" {
		e.notifySourceRefresh(changed)
	}
	if err != nil || selected == nil {
		return changed != "", err
	}
	r := *selected
	budget := 15 * time.Second
	if r.IncludeReadiness {
		budget = 45 * time.Second
	} else if r.IncludeCI {
		budget = 30 * time.Second
	}
	operationCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	readCtx, sourceCancel := context.WithTimeout(operationCtx, 15*time.Second)
	plan, readErr := ResolveSourceObservation(readCtx, readers[r.capability], r.repositoryID, configuration, r.PullNumber, r.HeadSHA)
	sourceCancel()
	var ci *reviewprovider.CIObservation
	if readErr == nil && (r.IncludeCI || r.IncludeReadiness) && ctx.Err() == nil {
		ci = observeSourceCI(operationCtx, readers[r.capability], plan)
	}
	var review *reviewprovider.PullReviewObservation
	if readErr == nil && r.IncludeReadiness && ctx.Err() == nil {
		review = observeSourceReviewState(operationCtx, readers[r.capability], plan)
	}
	if ctx.Err() != nil {
		return true, nil
	}
	reviewBody := encoded(review)
	ciBody := encoded(ci) // Serialize bounded private inventories before taking the writer.
	err = retrySourceRefreshLocal(ctx, func() error {
		return e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			current, err := readSourceRefresh(ctx, tx, r.ProjectID, r.ID)
			if err != nil {
				return err
			}
			if current.State != r.State || current.Revision != r.Revision {
				return conflict()
			}
			observation := SourceObservation{RepositoryID: plan.Repository.ID, RepositoryName: plan.Repository.FullName, PullNumber: plan.Pull.Number, HeadSHA: plan.Pull.HeadSHA, BaseSHA: plan.Pull.BaseSHA, BaseRef: plan.Pull.BaseRef, ParentPullNumber: plan.ParentPullNumber, ParentSHA: plan.ParentSHA, ObservedAt: plan.ObservedAt}
			checked := r
			if readErr == nil {
				checked.Observation = &observation
			}
			_, reason, err := e.refreshBinding(ctx, tx, checked)
			if err != nil {
				return err
			}
			if reason == "" && readErr != nil {
				reason = sourceRefreshReason(readErr)
			}
			state := "held"
			var retained *SourceObservation
			if reason == "" {
				if !validSourceObservation(observation) || observation.RepositoryID != r.repositoryID || observation.PullNumber != r.PullNumber || observation.HeadSHA != r.HeadSHA {
					reason = "source_unavailable"
				} else {
					state, retained = "completed", &observation
				}
			}
			retainedCI, retainedReview := "null", "null"
			if state == "completed" {
				retainedCI, retainedReview = ciBody, reviewBody
				if err = publishSourceRefreshStatus(ctx, tx, core.Scope{ProjectID: r.ProjectID, PrincipalID: r.actor, WorkerID: r.worker}, r.repositoryID, metadataPins, plan, ci, review); err != nil {
					return err
				}
			}
			return e.updateSourceRefresh(ctx, tx, r, state, reason, retained, retainedCI, retainedReview)
		})
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
	SourceObligationsPending int64                     `json:"source_obligations_pending,omitempty"`
	IncomingRoutesPending    int64                     `json:"incoming_routes_pending,omitempty"`
	IncomingRoutesCapacity   bool                      `json:"incoming_routes_capacity,omitempty"`
	ProjectID                string                    `json:"project_id"`
	RefreshID                string                    `json:"refresh_id"`
	Source                   string                    `json:"source"`
	ConfigurationRevision    int64                     `json:"configuration_revision"`
	PolicyRevision           int64                     `json:"policy_revision"`
	SourceObservedAt         string                    `json:"source_observed_at"`
	EvaluatedAt              string                    `json:"evaluated_at"`
	State                    string                    `json:"state"`
	Reason                   string                    `json:"reason,omitempty"`
	Target                   SourceObservation         `json:"target"`
	Channels                 []PolicyAssessmentChannel `json:"channels"`
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
		r, err := readSourceRefreshMetadata(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return err
		}
		observation := *r.Observation
		result = SourcePolicyAssessment{ProjectID: scope.ProjectID, RefreshID: r.ID, Source: "observed_comparison", ConfigurationRevision: r.ConfigurationRevision, SourceObservedAt: observation.ObservedAt, EvaluatedAt: stamp(), State: "unconfigured", Target: observation, Channels: []PolicyAssessmentChannel{}}
		// Applicability is shared with CI/readiness; current reader authorization
		// remains independent of the actor who originally collected the source.
		revision, bindingReason, err := e.assessmentSourceBinding(ctx, tx, r)
		if err != nil {
			return err
		}
		result.ConfigurationRevision = revision
		stale := bindingReason != ""
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
		result.IncomingRoutesPending, result.IncomingRoutesCapacity, err = incomingRoutePolicyFacts(ctx, tx, scope.ProjectID, number)
		if err != nil {
			return err
		}
		if result.IncomingRoutesCapacity {
			result.State, result.Reason = "held", "routed_findings_capacity"
		} else if result.IncomingRoutesPending > 0 {
			result.State, result.Reason = "held", "routed_findings_pending"
		}
		result.SourceObligationsPending, err = sourceObligationCount(ctx, tx, scope.ProjectID, number)
		if err != nil {
			return err
		}
		if result.SourceObligationsPending > 0 {
			result.State = "held"
			if result.Reason == "" {
				result.Reason = "source_obligations_pending"
			}
		}
		for _, channel := range policy.Policy.RequiredChannels {
			state, err := channelProgress(ctx, tx, scope.ProjectID, r.repositoryID, number, channel)
			if err != nil {
				return err
			}
			result.Channels = append(result.Channels, PolicyAssessmentChannel{Channel: channel, Covered: state.Covered, Pending: state.Pending, RoundsConfigured: state.RoundsConfigured, RoundsComplete: state.RoundsComplete})
			if !state.Covered || state.Pending || state.RoundsConfigured && !state.RoundsComplete {
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
	if ctx == nil || e == nil || e.store == nil || !validRefreshID(id) || !identifier.MatchString(scope.PrincipalID) || maxWait < 0 || maxWait > 900*time.Second {
		return SourceRefreshOperation{}, invalid()
	}
	deadline := time.Now().Add(maxWait)
	waitCtx, cleanupWait := e.resultWaitContext(ctx)
	defer cleanupWait()
	canceled := func() error {
		return resultWaitCanceled(waitCtx, "The source refresh wait was canceled; its retained operation is unchanged.")
	}
	read := func() (SourceRefreshOperation, error) {
		if waitCtx.Err() != nil {
			return SourceRefreshOperation{}, canceled()
		}
		var value SourceRefreshOperation
		err := core.RetryReadAdmission(waitCtx, deadline, func(readCtx context.Context) error {
			var readErr error
			value, readErr = e.ReadSourceRefresh(readCtx, scope, id)
			return readErr
		})
		if waitCtx.Err() != nil {
			return SourceRefreshOperation{}, canceled()
		}
		return value, err
	}
	if maxWait == 0 {
		return read()
	}
	if waitCtx.Err() != nil {
		return SourceRefreshOperation{}, canceled()
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
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		value, err := read()
		if err != nil {
			return SourceRefreshOperation{}, err
		}
		if value.State == "completed" || value.State == "held" {
			return value, nil
		}
		select {
		case <-waitCtx.Done():
			return SourceRefreshOperation{}, canceled()
		case <-timer.C:
			return read()
		case <-waiter.changed:
		}
	}
}
