package reviewexec

import (
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
	"github.com/benhook1013/FireController/internal/strictjson"
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

// Distinguish invalid stored scheduling records from project-local history
// failures without changing public errors or outer project isolation.
type schedulingStateError struct{ error }

func (e *schedulingStateError) Unwrap() error { return e.error }

func schedulingUnavailable() error {
	return automaticProjectStateFailure("storage_unavailable", "Stored review scheduling is unavailable.", 503)
}

func schedulingRecordUnavailable() error {
	return &schedulingStateError{schedulingUnavailable()}
}

func schedulingRefused(err error) bool {
	var scheduling *schedulingStateError
	return errors.As(err, &scheduling)
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
		return s, schedulingRecordUnavailable()
	}
	s.AutomaticHosted, s.AutomaticCLI = hostedValue == 1, cliValue == 1
	if s.Revision < 1 || !identifier.MatchString(s.actor) || !roundTime(s.UpdatedAt) || (s.AutomaticHosted || s.AutomaticCLI) && (!identifier.MatchString(s.AuthorizingWorkerID) || !identifier.MatchString(s.credential)) {
		return s, schedulingRecordUnavailable()
	}
	return s, nil
}

type schedulingEvent struct {
	Hosted      *bool           `json:"automatic_hosted"`
	CLI         *bool           `json:"automatic_cli"`
	HostedBasis json.RawMessage `json:"hosted_basis_revision"`
	CLIBasis    json.RawMessage `json:"cli_basis_revision"`
}

// Channel basis revisions are carried in the current scheduling event, not in
// the authorization revision. Legacy events retain the shared revision verbatim:
// deploying this change must not regenerate cancelled or failed intent digests.
func schedulingBasisRevisions(ctx context.Context, tx *sql.Tx, s schedulingRecord) (hosted, cli int64, err error) {
	if s.Revision == 0 {
		return 0, 0, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT actor_id,CASE WHEN typeof(payload_json)='text' AND length(CAST(payload_json AS BLOB))<=4096 THEN payload_json END FROM events WHERE project_id=? AND event_type='review.scheduling_updated' AND resource_id=? AND revision=? LIMIT 2`, s.ProjectID, s.ProjectID, s.Revision)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		var actor string
		var payload sql.NullString
		if err = rows.Scan(&actor, &payload); err != nil {
			return 0, 0, err
		}
		var event schedulingEvent
		if count != 1 || actor != s.actor || !payload.Valid || strictjson.Decode([]byte(payload.String), &event) != nil || event.Hosted == nil || *event.Hosted != s.AutomaticHosted || event.CLI == nil && s.AutomaticCLI || event.CLI != nil && *event.CLI != s.AutomaticCLI {
			return 0, 0, schedulingUnavailable()
		}
		hosted, cli = s.Revision, s.Revision
		if len(event.HostedBasis) != 0 || len(event.CLIBasis) != 0 {
			if json.Unmarshal(event.HostedBasis, &hosted) != nil || json.Unmarshal(event.CLIBasis, &cli) != nil || string(event.HostedBasis) == "null" || string(event.CLIBasis) == "null" || hosted < 1 || cli < 1 || hosted > s.Revision || cli > s.Revision {
				return 0, 0, schedulingUnavailable()
			}
		}
	}
	if err = rows.Err(); err != nil {
		return 0, 0, err
	}
	if err = rows.Close(); err != nil {
		return 0, 0, err
	}
	if count != 1 {
		return 0, 0, schedulingUnavailable()
	}
	return hosted, cli, nil
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

// A pull-specific retained control refusal is distinct from project
// scheduling, a query error, or another member of the queue.
type pullSchedulingStateError struct {
	err     error
	project string
	pull    int64
}

func (e *pullSchedulingStateError) Error() string { return e.err.Error() }
func (e *pullSchedulingStateError) Unwrap() error { return e.err }

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
		return s, &pullSchedulingStateError{schedulingRecordUnavailable(), project, pull}
	}
	s.Paused = pausedValue == 1
	if s.Revision < 1 || !identifier.MatchString(s.ActorID) || !roundTime(s.UpdatedAt) || !validSchedulingReason(s.Reason) || !s.Paused && s.Reason != "" {
		return s, &pullSchedulingStateError{schedulingRecordUnavailable(), project, pull}
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
