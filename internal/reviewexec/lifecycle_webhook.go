package reviewexec

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/githubhook"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type lifecycleHint struct {
	wanted, done uint64
	attemptAt    time.Time
	head         string
}

// HintGitHub queues acquisition, never provider facts. Only already enrolled,
// visible PRs in the asserted stable repository can receive these local hints.
func (e *Engine) HintGitHub(ctx context.Context, hint githubhook.Hint) error {
	if ctx == nil || e == nil || e.store == nil {
		return invalid()
	}
	if hint.RepositoryID <= 0 || !hint.Metadata && !hint.CI || len(hint.PullNumbers) > 100 || hint.HeadSHA != "" && !sha.MatchString(hint.HeadSHA) {
		return nil
	}
	type match struct {
		key          lifecycleKey
		metadata, ci bool
		head         string
	}
	var matches []match
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT p.id,c.body_json FROM projects p JOIN review_execution_config c ON c.project_id=p.id AND c.revision=(SELECT MAX(current.revision) FROM review_execution_config current WHERE current.project_id=p.id) WHERE p.github_repository_id=? ORDER BY p.id LIMIT 65`, hint.RepositoryID)
		if err != nil {
			return err
		}
		type project struct {
			id string
			c  Configuration
		}
		var projects []project
		for rows.Next() {
			var p project
			var body string
			if err = rows.Scan(&p.id, &body); err != nil {
				rows.Close()
				return err
			}
			if len(projects) == 64 || len(body) > 8192 || strictjson.Decode([]byte(body), &p.c) != nil || validateConfigurationShape(p.c) != nil {
				rows.Close()
				return lifecycleHintUnavailable()
			}
			projects = append(projects, p)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, p := range projects {
			if len(p.c.Queue) == 0 {
				continue
			}
			args := []any{p.id}
			slots := make([]string, len(p.c.Queue))
			for i, number := range p.c.Queue {
				slots[i] = "?"
				args = append(args, number)
			}
			rows, err := tx.QueryContext(ctx, `SELECT p.number,v.head_sha,v.base_ref FROM review_current_pulls p JOIN review_pull_memberships m ON m.project_id=p.project_id AND m.number=p.number AND m.start_revision=p.start_revision JOIN review_pull_versions v ON v.project_id=m.project_id AND v.number=m.number AND v.version_revision=m.version_revision WHERE p.project_id=? AND p.number IN (`+strings.Join(slots, ",")+`) AND NOT EXISTS(SELECT 1 FROM review_current_retired r WHERE r.project_id=p.project_id AND r.number=p.number) ORDER BY p.position LIMIT 100`, args...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var number int64
				var head, base string
				if err = rows.Scan(&number, &head, &base); err != nil {
					rows.Close()
					return err
				}
				if !slices.Contains(p.c.Queue, number) {
					continue
				}
				sameHead := hint.HeadSHA != "" && hint.HeadSHA == head
				basePush := hint.BranchRef != "" && hint.BranchRef == base
				metadata := hint.Metadata && (slices.Contains(hint.PullNumbers, number) || sameHead || basePush)
				// A CI-only event cannot use its explicit PR number to refresh
				// a different head. A base push concerns that PR's current merge.
				ci := hint.CI && (sameHead || basePush)
				if metadata || ci {
					matches = append(matches, match{lifecycleKey{p.id, number}, metadata, ci, head})
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return lifecycleHintUnavailable()
	}
	if len(matches) == 0 {
		return nil
	}
	e.lifecycleMu.RLock()
	s := e.lifecycleSchedule
	e.lifecycleMu.RUnlock()
	if s == nil || !e.lifecycleRunning.Load() {
		return lifecycleHintUnavailable()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	extra := 0
	for _, match := range matches {
		if _, exists := s.due[match.key]; !exists {
			extra++
		}
	}
	if len(s.due)+extra > 64*100 {
		return lifecycleHintUnavailable()
	}
	for _, match := range matches {
		due := s.due[match.key]
		if match.metadata {
			due.hints[lifecycleMetadata].wanted++
		}
		if match.ci {
			due.hints[lifecycleEnrichment].wanted++
			due.hints[lifecycleEnrichment].head = match.head
		}
		s.due[match.key] = due
	}
	return nil
}

func lifecycleHintUnavailable() error {
	return core.Fail("overloaded", "GitHub observation hints cannot currently be scheduled.", 503)
}
