package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"math"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// This shape exists only to validate immutable pre-removal policy history and
// upgrade its current revision. It is never accepted by the policy API.
type legacyPolicy struct {
	RequiredChannels []string `json:"required_channels"`
	PausedChannels   []string `json:"paused_channels"`
	MaxReviewFiles   int64    `json:"max_review_files"`
}

func decodeRetainedPolicy(body string) (Policy, []string, error) {
	if len(body) > 1024 {
		return Policy{}, nil, errors.New("invalid retained policy")
	}
	var current Policy
	if strictjson.Decode([]byte(body), &current) == nil && validPolicy(current) {
		return current, nil, nil
	}
	var old legacyPolicy
	if strictjson.Decode([]byte(body), &old) != nil || old.PausedChannels == nil || len(old.PausedChannels) > 2 {
		return Policy{}, nil, errors.New("invalid retained policy")
	}
	current = Policy{RequiredChannels: old.RequiredChannels, MaxReviewFiles: old.MaxReviewFiles}
	if !validPolicy(current) {
		return Policy{}, nil, errors.New("invalid retained policy")
	}
	seen := map[string]bool{}
	for _, channel := range old.PausedChannels {
		if !validChannel(channel) || seen[channel] {
			return Policy{}, nil, errors.New("invalid retained policy")
		}
		seen[channel] = true
	}
	return current, old.PausedChannels, nil
}

// Migration shares the exclusive schema transaction. It appends canonical
// current revisions while preserving original history, receipts and bindings.
// An existing channel stop becomes the single project-wide execution stop;
// an unconfigured project stays unconfigured and cannot dispatch reviews.
func migratePolicyChannelPauses(ctx context.Context, conn *sql.Conn) error {
	type retained struct {
		project  string
		revision int64
		body     string
	}
	rows, err := conn.QueryContext(ctx, `SELECT p.project_id,p.revision,CASE WHEN typeof(p.body_json)='text' AND length(CAST(p.body_json AS BLOB))<=1024 THEN p.body_json END FROM review_policy_config p WHERE p.revision=(SELECT max(v.revision) FROM review_policy_config v WHERE v.project_id=p.project_id) ORDER BY p.project_id LIMIT 10001`)
	if err != nil {
		return err
	}
	items := []retained{}
	for rows.Next() {
		var item retained
		if err = rows.Scan(&item.project, &item.revision, &item.body); err != nil {
			break
		}
		items = append(items, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if len(items) > 10000 {
		return errors.New("policy migration capacity exceeded")
	}
	var executionExists bool
	if err = conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='review_execution_config')`).Scan(&executionExists); err != nil {
		return err
	}
	const actor = "system_review_channel_pause_removal"
	for _, item := range items {
		policy, pauses, err := decodeRetainedPolicy(item.body)
		if err != nil {
			return err
		}
		if pauses == nil {
			continue
		}
		if item.revision < 1 || item.revision == math.MaxInt64 {
			return errors.New("policy migration revision unavailable")
		}
		at := stamp()
		if len(pauses) > 0 && executionExists {
			var body string
			var revision int64
			err = conn.QueryRowContext(ctx, `SELECT revision,CASE WHEN typeof(body_json)='text' AND length(CAST(body_json AS BLOB))<=32768 THEN body_json END FROM review_execution_config WHERE project_id=? ORDER BY revision DESC LIMIT 1`, item.project).Scan(&revision, &body)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				var configuration Configuration
				if strictjson.Decode([]byte(body), &configuration) != nil || validateConfigurationShape(configuration) != nil || *configuration.ExpectedRevision != revision-1 {
					return errors.New("execution migration configuration unavailable")
				}
				if !configuration.Paused {
					if revision == math.MaxInt64 {
						return errors.New("execution migration revision unavailable")
					}
					configuration.ExpectedRevision = &revision
					configuration.Paused = true
					if _, err = conn.ExecContext(ctx, `INSERT INTO review_execution_config(project_id,revision,body_json) VALUES(?,?,?)`, item.project, revision+1, encoded(configuration)); err != nil {
						return err
					}
					if _, err = conn.ExecContext(ctx, `INSERT INTO events(project_id,event_type,resource_id,revision,actor_id,payload_json,created_at) VALUES(?,'reviews.configured',?,?,?,?,?)`, item.project, item.project, revision+1, actor, encoded(map[string]any{"revision": revision + 1, "paused": true}), at); err != nil {
						return err
					}
				}
			}
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO review_policy_config(project_id,revision,body_json,actor_id,created_at) VALUES(?,?,?,?,?)`, item.project, item.revision+1, encoded(policy), actor, at); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO events(project_id,event_type,resource_id,revision,actor_id,payload_json,created_at) VALUES(?,'reviews.policy_configured',?,?,?,?,?)`, item.project, item.project, item.revision+1, actor, encoded(map[string]any{"revision": item.revision + 1}), at); err != nil {
			return err
		}
	}
	return nil
}
