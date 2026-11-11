package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
)

type Scheduling struct {
	PausedPulls         []PullScheduling `json:"paused_pulls"`
	ProjectID           string           `json:"project_id"`
	Revision            int64            `json:"revision"`
	AutomaticCLI        bool             `json:"automatic_cli"`
	AutomaticHosted     bool             `json:"automatic_hosted"`
	AuthorizingWorkerID string           `json:"authorizing_worker_id,omitempty"`
	UpdatedAt           string           `json:"updated_at,omitempty"`
	Reason              string           `json:"reason,omitempty"`
}
type SchedulingRequest struct {
	ExpectedRevision *int64 `json:"expected_revision"`
	AutomaticCLI     *bool  `json:"automatic_cli,omitempty"`
	AutomaticHosted  *bool  `json:"automatic_hosted"`
}

// Omission preserves the CLI setting; explicit null is not a boolean choice.
func (r *SchedulingRequest) UnmarshalJSON(data []byte) error {
	type plain SchedulingRequest
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if raw, exists := fields["automatic_cli"]; exists && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return invalid()
	}
	*r = SchedulingRequest(value)
	return nil
}

type PullScheduling struct {
	ProjectID  string `json:"project_id"`
	PullNumber int64  `json:"pull_number"`
	Revision   int64  `json:"revision"`
	Paused     bool   `json:"paused"`
	Reason     string `json:"reason"`
	ActorID    string `json:"actor_id,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}
type PullSchedulingRequest struct {
	ExpectedRevision *int64 `json:"expected_revision"`
	Paused           *bool  `json:"paused"`
	Reason           string `json:"reason,omitempty"`
}
type schedulingRecord struct {
	Scheduling
	actor, credential string
}

func schedulingUnavailable() error {
	return automaticProjectStateFailure("storage_unavailable", "Stored review scheduling is unavailable.", 503)
}
func readScheduling(ctx context.Context, tx *sql.Tx, project string) (s schedulingRecord, err error) {
	s.ProjectID = project
	var revision, hosted, cli, actor, worker, credential, updated any
	err = tx.QueryRowContext(ctx, `SELECT revision,automatic_hosted,automatic_cli,actor_id,worker_id,credential_id,updated_at FROM review_scheduling WHERE project_id=?`, project).Scan(&revision, &hosted, &cli, &actor, &worker, &credential, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	// Scan into unconverted values so malformed SQLite column types can be
	// classified as retained data without disguising an actual database error.
	var revisionOK, actorOK, workerOK, credentialOK, updatedOK bool
	s.Revision, revisionOK = revision.(int64)
	hostedValue, hostedOK := hosted.(int64)
	cliValue, cliOK := cli.(int64)
	s.actor, actorOK = actor.(string)
	s.AuthorizingWorkerID, workerOK = worker.(string)
	s.credential, credentialOK = credential.(string)
	s.UpdatedAt, updatedOK = updated.(string)
	if !revisionOK || !hostedOK || !cliOK || !actorOK || !workerOK || !credentialOK || !updatedOK || hostedValue < 0 || hostedValue > 1 || cliValue < 0 || cliValue > 1 {
		return s, schedulingUnavailable()
	}
	s.AutomaticHosted, s.AutomaticCLI = hostedValue == 1, cliValue == 1
	if s.Revision < 1 || !identifier.MatchString(s.actor) || !roundTime(s.UpdatedAt) || (s.AutomaticHosted || s.AutomaticCLI) && (!identifier.MatchString(s.AuthorizingWorkerID) || !identifier.MatchString(s.credential)) {
		return s, schedulingUnavailable()
	}
	return s, nil
}
func schedulingAuthorizerAllowed(ctx context.Context, tx *sql.Tx, s schedulingRecord) (bool, error) {
	if !s.AutomaticHosted && !s.AutomaticCLI {
		return false, nil
	}
	var allowed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM credentials c WHERE c.id=? AND c.principal_id=? AND c.revoked=0 AND (c.is_admin=1 OR EXISTS(SELECT 1 FROM grants g WHERE g.credential_id=c.id AND g.project_id=? AND g.can_read=1 AND g.can_write=1 AND g.can_manage=1)) AND EXISTS(SELECT 1 FROM workers w WHERE w.project_id=? AND w.id=? AND NOT EXISTS(SELECT 1 FROM worker_retirements r WHERE r.project_id=w.project_id AND r.worker_id=w.id)))`, s.credential, s.actor, s.ProjectID, s.ProjectID, s.AuthorizingWorkerID).Scan(&allowed)
	return allowed, err
}
func (e *Engine) ReadScheduling(ctx context.Context, scope core.Scope) (Scheduling, error) {
	var out Scheduling
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		s, err := readScheduling(ctx, tx, scope.ProjectID)
		if err != nil {
			return err
		}
		out = s.Scheduling
		out.PausedPulls = []PullScheduling{}
		c, _, configErr := readConfiguration(ctx, tx, scope.ProjectID)
		if configErr != nil {
			var failure *core.Error
			if !errors.As(configErr, &failure) || failure.Code != "reviews_unconfigured" {
				return configErr
			}
		} else {
			if validateConfigurationShape(c) != nil {
				return schedulingUnavailable()
			}
			for _, pull := range c.Queue {
				p, err := readPullScheduling(ctx, tx, scope.ProjectID, pull)
				if err != nil {
					return err
				}
				if p.Paused {
					out.PausedPulls = append(out.PausedPulls, p)
				}
			}
		}
		if s.AutomaticHosted || s.AutomaticCLI {
			var held bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metadata WHERE (key='restore:provider_state' AND value='held') OR (key='restore:state' AND value='inactive'))`).Scan(&held); err != nil {
				return err
			}
			if held {
				out.Reason = "provider_hold"
				return nil
			}
			ok, err := schedulingAuthorizerAllowed(ctx, tx, s)
			if err != nil {
				return err
			}
			if !ok {
				out.Reason = "authorizer_unavailable"
			}
		}
		return nil
	})
	return out, err
}
func validSchedulingRevision(r *int64) bool { return r != nil && *r >= 0 && *r < math.MaxInt64 }
func (e *Engine) ConfigureScheduling(ctx context.Context, scope core.Scope, key string, input SchedulingRequest) (json.RawMessage, error) {
	if !validSchedulingRevision(input.ExpectedRevision) || input.AutomaticHosted == nil {
		return nil, invalid()
	}
	scope = scope.RequireAdministration()
	out, err := e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.scheduling", Target: "scheduling", Key: key, Request: input}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		old, err := readScheduling(ctx, tx, scope.ProjectID)
		if err != nil {
			return nil, err
		}
		if old.Revision != *input.ExpectedRevision {
			return nil, conflict()
		}
		automaticCLI := old.AutomaticCLI
		if input.AutomaticCLI != nil {
			automaticCLI = *input.AutomaticCLI
		}
		credential := ""
		if *input.AutomaticHosted || automaticCLI {
			if err := core.CheckActiveWorker(ctx, tx, scope.ProjectID, scope.WorkerID); err != nil {
				return nil, err
			}
			credential, err = e.store.AuthenticatedCredentialID(ctx, tx, scope)
			if err != nil {
				return nil, err
			}
			allowed, err := schedulingAuthorizerAllowed(ctx, tx, schedulingRecord{Scheduling: Scheduling{ProjectID: scope.ProjectID, AutomaticHosted: true, AuthorizingWorkerID: scope.WorkerID}, actor: scope.PrincipalID, credential: credential})
			if err != nil {
				return nil, err
			}
			if !allowed {
				return nil, core.Fail("forbidden", "An active bound project manager must authorize automatic review scheduling.", 403)
			}
		}
		at := stamp()
		worker := scope.WorkerID
		_, err = tx.ExecContext(ctx, `INSERT INTO review_scheduling(project_id,revision,automatic_hosted,automatic_cli,actor_id,worker_id,credential_id,updated_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(project_id) DO UPDATE SET revision=excluded.revision,automatic_hosted=excluded.automatic_hosted,automatic_cli=excluded.automatic_cli,actor_id=excluded.actor_id,worker_id=excluded.worker_id,credential_id=excluded.credential_id,updated_at=excluded.updated_at`, scope.ProjectID, old.Revision+1, *input.AutomaticHosted, automaticCLI, scope.PrincipalID, worker, credential, at)
		if err != nil {
			return nil, err
		}
		if err = core.AppendEvent(ctx, tx, scope, "review.scheduling_updated", scope.ProjectID, old.Revision+1, map[string]any{"automatic_hosted": *input.AutomaticHosted, "automatic_cli": automaticCLI}); err != nil {
			return nil, err
		}
		return Receipt{ID: scope.ProjectID, State: "scheduling_configured", Revision: old.Revision + 1}, nil
	})
	if err == nil {
		e.wakeScheduling()
	}
	return out, err
}
func readPullScheduling(ctx context.Context, tx *sql.Tx, project string, pull int64) (s PullScheduling, err error) {
	s.ProjectID = project
	s.PullNumber = pull
	var revision, paused, reason, actor, updated any
	err = tx.QueryRowContext(ctx, `SELECT revision,paused,reason,actor_id,updated_at FROM review_pull_scheduling WHERE project_id=? AND pull_number=?`, project, pull).Scan(&revision, &paused, &reason, &actor, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	var revisionOK, reasonOK, actorOK, updatedOK bool
	s.Revision, revisionOK = revision.(int64)
	pausedValue, pausedOK := paused.(int64)
	s.Reason, reasonOK = reason.(string)
	s.ActorID, actorOK = actor.(string)
	s.UpdatedAt, updatedOK = updated.(string)
	if !revisionOK || !pausedOK || !reasonOK || !actorOK || !updatedOK || pausedValue < 0 || pausedValue > 1 {
		return s, schedulingUnavailable()
	}
	s.Paused = pausedValue == 1
	if s.Revision < 1 || !identifier.MatchString(s.ActorID) || !roundTime(s.UpdatedAt) || !validSchedulingReason(s.Reason) || !s.Paused && s.Reason != "" {
		return s, schedulingUnavailable()
	}
	return s, nil
}
func validSchedulingReason(s string) bool {
	return len(s) <= 256 && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}
func (e *Engine) ReadPullScheduling(ctx context.Context, scope core.Scope, pull int64) (PullScheduling, error) {
	if pull < 1 || pull > 2147483647 {
		return PullScheduling{}, invalid()
	}
	var out PullScheduling
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		out, err = readPullScheduling(ctx, tx, scope.ProjectID, pull)
		return err
	})
	return out, err
}
func (e *Engine) ConfigurePullScheduling(ctx context.Context, scope core.Scope, key string, pull int64, input PullSchedulingRequest) (json.RawMessage, error) {
	if pull < 1 || pull > 2147483647 || !validSchedulingRevision(input.ExpectedRevision) || input.Paused == nil || !validSchedulingReason(input.Reason) {
		return nil, invalid()
	}
	out, err := e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.pull_scheduling", Target: strconv.FormatInt(pull, 10), Key: key, Request: input}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		old, err := readPullScheduling(ctx, tx, scope.ProjectID, pull)
		if err != nil {
			return nil, err
		}
		if old.Revision != *input.ExpectedRevision {
			return nil, conflict()
		}
		reason := input.Reason
		if !*input.Paused {
			reason = ""
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO review_pull_scheduling(project_id,pull_number,revision,paused,reason,actor_id,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(project_id,pull_number) DO UPDATE SET revision=excluded.revision,paused=excluded.paused,reason=excluded.reason,actor_id=excluded.actor_id,updated_at=excluded.updated_at`, scope.ProjectID, pull, old.Revision+1, *input.Paused, reason, scope.PrincipalID, stamp())
		if err != nil {
			return nil, err
		}
		if err = core.AppendEvent(ctx, tx, scope, "review.pull_scheduling_updated", strconv.FormatInt(pull, 10), old.Revision+1, map[string]any{"pull_number": pull, "paused": *input.Paused}); err != nil {
			return nil, err
		}
		return Receipt{ID: strconv.FormatInt(pull, 10), State: "pull_scheduling_updated", Revision: old.Revision + 1}, nil
	})
	if err == nil {
		e.wakeScheduling()
	}
	return out, err
}
func (e *Engine) wakeScheduling() {
	select {
	case e.runWake <- struct{}{}:
	default:
	}
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func requirePullScheduling(ctx context.Context, tx *sql.Tx, project string, pull int64) error {
	s, err := readPullScheduling(ctx, tx, project, pull)
	if err != nil {
		return err
	}
	if s.Paused {
		return core.Fail("pull_scheduling_paused", "New review admission is paused for this PR; existing issued reviews can still be collected.", 409)
	}
	return nil
}
