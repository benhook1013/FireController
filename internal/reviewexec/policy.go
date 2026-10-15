package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// PolicyConfiguration is an explicit project preference, not a provider quota.
// A zero MaxReviewFiles adds no project ceiling and asserts no provider limit.
type PolicyConfiguration struct {
	ExpectedRevision *int64   `json:"expected_revision"`
	RequiredChannels []string `json:"required_channels"`
	PausedChannels   []string `json:"paused_channels"`
	MaxReviewFiles   int64    `json:"max_review_files"`
}

// Policy omits the previous-revision assertion from the current stored values.
type Policy struct {
	RequiredChannels []string `json:"required_channels"`
	PausedChannels   []string `json:"paused_channels"`
	MaxReviewFiles   int64    `json:"max_review_files"`
}

type PolicyRecord struct {
	ProjectID  string `json:"project_id"`
	Revision   int64  `json:"revision"`
	ActorID    string `json:"actor_id"`
	RecordedAt string `json:"recorded_at"`
	Policy     Policy `json:"policy"`
}

func validPolicy(policy Policy) bool {
	if policy.RequiredChannels == nil || len(policy.RequiredChannels) < 1 || len(policy.RequiredChannels) > 2 || policy.PausedChannels == nil || len(policy.PausedChannels) > 2 || policy.MaxReviewFiles < 0 || policy.MaxReviewFiles > 100000 {
		return false
	}
	for _, channels := range [][]string{policy.RequiredChannels, policy.PausedChannels} {
		seen := map[string]bool{}
		for _, channel := range channels {
			if !validChannel(channel) || seen[channel] {
				return false
			}
			seen[channel] = true
		}
	}
	return true
}

func clonePolicyConfiguration(value PolicyConfiguration) PolicyConfiguration {
	if value.ExpectedRevision != nil {
		revision := *value.ExpectedRevision
		value.ExpectedRevision = &revision
	}
	if value.RequiredChannels != nil {
		channels := make([]string, len(value.RequiredChannels))
		copy(channels, value.RequiredChannels)
		value.RequiredChannels = channels
	}
	if value.PausedChannels != nil {
		channels := make([]string, len(value.PausedChannels))
		copy(channels, value.PausedChannels)
		value.PausedChannels = channels
	}
	return value
}

// ConfigurePolicy stores one immutable administrator-attributed revision. It
// performs no admission, provider observation, execution or policy assessment.
func (e *Engine) ConfigurePolicy(ctx context.Context, scope core.Scope, key string, input PolicyConfiguration) (json.RawMessage, error) {
	// Bound collection copying even for trusted direct callers.
	if len(input.RequiredChannels) > 2 || len(input.PausedChannels) > 2 {
		return nil, invalid()
	}
	input = clonePolicyConfiguration(input)
	policy := Policy{RequiredChannels: input.RequiredChannels, PausedChannels: input.PausedChannels, MaxReviewFiles: input.MaxReviewFiles}
	if !identifier.MatchString(scope.ProjectID) || input.ExpectedRevision == nil || *input.ExpectedRevision < 0 || *input.ExpectedRevision == math.MaxInt64 || !validPolicy(policy) {
		return nil, invalid()
	}
	return e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.policy.configure", Target: "policy", Key: key, Request: input}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		// A write-capable scope is insufficient even for direct Go callers.
		var admin int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM credentials WHERE principal_id=? AND is_admin=1 AND revoked=0`, scope.PrincipalID).Scan(&admin); err != nil {
			return nil, err
		}
		if admin != 1 {
			return nil, core.Fail("forbidden", "Administrative permission is required.", 403)
		}
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM review_policy_config WHERE project_id=?`, scope.ProjectID).Scan(&revision); err != nil {
			return nil, err
		}
		if revision != *input.ExpectedRevision || revision == math.MaxInt64 {
			return nil, conflict()
		}
		revision++
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_policy_config(project_id,revision,body_json,actor_id,created_at) VALUES(?,?,?,?,?)`, scope.ProjectID, revision, encoded(policy), scope.PrincipalID, stamp()); err != nil {
			return nil, err
		}
		if err := core.AppendEvent(ctx, tx, scope, "reviews.policy_configured", scope.ProjectID, revision, struct {
			Revision int64 `json:"revision"`
		}{revision}); err != nil {
			return nil, err
		}
		return Receipt{ID: scope.ProjectID, State: "policy_configured", Revision: revision}, nil
	})
}

func readPolicy(ctx context.Context, tx *sql.Tx, project string) (PolicyRecord, error) {
	var record PolicyRecord
	var body string
	err := tx.QueryRowContext(ctx, `SELECT project_id,revision,actor_id,created_at,body_json FROM review_policy_config WHERE project_id=? ORDER BY revision DESC LIMIT 1`, project).Scan(&record.ProjectID, &record.Revision, &record.ActorID, &record.RecordedAt, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return PolicyRecord{}, core.Fail("reviews_policy_unconfigured", "Configure explicit project review requirements before assessing policy.", 409)
	}
	if err != nil {
		return PolicyRecord{}, err
	}
	if len(body) > 1024 || strictjson.Decode([]byte(body), &record.Policy) != nil || !validPolicy(record.Policy) {
		return PolicyRecord{}, core.Fail("storage_unavailable", "Stored project review policy is unavailable.", 500)
	}
	return record, nil
}

func (e *Engine) ReadPolicy(ctx context.Context, scope core.Scope) (PolicyRecord, error) {
	if !identifier.MatchString(scope.ProjectID) {
		return PolicyRecord{}, invalid()
	}
	var record PolicyRecord
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		record, err = readPolicy(ctx, tx, scope.ProjectID)
		return err
	})
	if err != nil {
		return PolicyRecord{}, err
	}
	return record, nil
}
