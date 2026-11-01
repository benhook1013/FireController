package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func automaticDenied() error {
	return core.Fail("automation_authorization_changed", "Automatic Hosted scheduling is disabled or its original authorizer is no longer authorized.", 403)
}
func (e *Engine) requireAutomaticRun(ctx context.Context, tx *sql.Tx, r Run) error {
	var revision int64
	var actor, worker, credential string
	err := tx.QueryRowContext(ctx, `SELECT scheduling_revision,actor_id,worker_id,credential_id FROM review_automatic_selections WHERE project_id=? AND selection_id=?`, r.ProjectID, r.ID).Scan(&revision, &actor, &worker, &credential)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	s, err := readScheduling(ctx, tx, r.ProjectID)
	if err != nil {
		return err
	}
	if revision > s.Revision || !s.AutomaticHosted || s.actor != actor || s.AuthorizingWorkerID != worker || s.credential != credential || r.ActorID != actor || r.WorkerID != worker || r.Channel != "hosted" {
		return automaticDenied()
	}
	_, err = e.store.ProjectAutomationScope(ctx, tx, r.ProjectID, actor, worker, credential)
	var failure *core.Error
	if errors.As(err, &failure) && failure.Status >= 400 && failure.Status < 500 {
		return automaticDenied()
	}
	return err
}
func (e *Engine) requireAutomaticOperation(ctx context.Context, tx *sql.Tx, o Operation) error {
	var selection string
	err := tx.QueryRowContext(ctx, `SELECT s.id FROM review_selections s JOIN review_automatic_selections a ON a.selection_id=s.id AND a.project_id=s.project_id WHERE s.project_id=? AND s.operation_id=?`, o.ProjectID, o.ID).Scan(&selection)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	r, err := readRun(ctx, tx, o.ProjectID, selection)
	if err != nil {
		return err
	}
	if r.OperationID != o.ID || r.ActorID != o.ActorID || r.WorkerID != o.WorkerID || r.Channel != o.Channel {
		return schedulingUnavailable()
	}
	return e.requireAutomaticRun(ctx, tx, r)
}

// The semantic digest excludes projection revisions, observation timestamps,
// attempt states and selection/operation transitions. Failed/cancelled requests
// therefore cannot create their own next automatic request. Only original source
// facts and deliberate controls, completed results and finding work change it.
func automaticPullBasis(ctx context.Context, tx *sql.Tx, project string, pull int64) (string, error) {
	h := sha256.New()
	appendRows := func(query string, args ...any) error {
		h.Write([]byte(query))
		h.Write([]byte{0})
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		count := 0
		for rows.Next() {
			count++
			if count > 10000 {
				return core.Fail("overloaded", "Automatic scheduling needs a narrower retained review scope.", 503)
			}
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err = rows.Scan(ptrs...); err != nil {
				return err
			}
			for i, v := range values {
				if b, ok := v.([]byte); ok {
					values[i] = string(b)
				}
			}
			b, err := json.Marshal(values)
			if err != nil {
				return err
			}
			h.Write(b)
			h.Write([]byte{'\n'})
		}
		if err = rows.Err(); err != nil {
			return err
		}
		return rows.Close()
	}
	queries := []struct {
		sql  string
		args []any
	}{
		{`SELECT revision,automatic_hosted FROM review_scheduling WHERE project_id=?`, []any{project}},
		{`SELECT revision FROM review_execution_config WHERE project_id=? ORDER BY revision DESC LIMIT 1`, []any{project}},
		{`SELECT revision FROM review_policy_config WHERE project_id=? ORDER BY revision DESC LIMIT 1`, []any{project}},
		{`SELECT revision,paused,reason FROM review_pull_scheduling WHERE project_id=? AND pull_number=?`, []any{project, pull}},
		{`SELECT epoch,grant_revision,closed,minimum,maximum,maximum_unbounded,used,dry,tapered FROM review_round_controls WHERE project_id=? AND pull_number=? AND channel='hosted'`, []any{project, pull}},
		{`SELECT a.evidence_id,a.revision FROM review_adjudication_state a JOIN review_execution_evidence e ON e.id=a.evidence_id AND e.project_id=a.project_id JOIN review_operations o ON o.id=e.operation_id AND o.project_id=e.project_id WHERE a.project_id=? AND o.pull_number=? AND (o.channel='hosted' OR json_array_length(json_extract(e.body_json,'$.capture.review.Findings'))>0) ORDER BY a.evidence_id LIMIT 10001`, []any{project, pull}},
		{`SELECT id,revision,state,disposition,has_fix FROM review_finding_routes WHERE project_id=? AND (source_pull_number=? OR target_pull_number=?) ORDER BY id LIMIT 10001`, []any{project, pull, pull}},
		{`SELECT o.id,o.revision,o.status FROM review_adoption_targets t JOIN review_adoption_obligations o ON o.id=t.obligation_id AND o.project_id=t.project_id WHERE t.project_id=? AND t.pull_number=? ORDER BY o.id LIMIT 10001`, []any{project, pull}},
	}
	for _, q := range queries {
		if err := appendRows(q.sql, q.args...); err != nil {
			return "", err
		}
	}
	var hasWorkspace bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='review_current_pulls' AND type='table')`).Scan(&hasWorkspace); err != nil {
		return "", err
	}
	if hasWorkspace {
		if err := appendRows(`SELECT p.state,p.head_sha,p.base_ref,p.base_sha,json_extract(p.metadata_json,'$.is_draft') FROM review_current_pulls c JOIN review_pull_memberships m ON m.project_id=c.project_id AND m.number=c.number AND m.start_revision=c.start_revision JOIN review_pull_versions p ON p.project_id=m.project_id AND p.number=m.number AND p.version_revision=m.version_revision WHERE c.project_id=? AND c.number=?`, project, pull); err != nil {
			return "", err
		}
		if err := appendRows(`SELECT v.id,v.channel,v.head_sha,v.complete,v.finalized,v.found,v.accepted,v.routed,v.fixed FROM review_current_runs c JOIN review_run_memberships m ON m.project_id=c.project_id AND m.id=c.id AND m.start_revision=c.start_revision JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision WHERE c.project_id=? AND c.pull_number=? AND v.channel='hosted' AND v.outcome='completed' ORDER BY v.id LIMIT 10001`, project, pull); err != nil {
			return "", err
		}
		if err := appendRows(`SELECT f.run_id,f.source_finding_id,f.disposition,f.fix_sha,f.proof_note,f.route_json FROM review_current_runs c JOIN review_run_memberships m ON m.project_id=c.project_id AND m.id=c.id AND m.start_revision=c.start_revision JOIN review_finding_versions f ON f.project_id=m.project_id AND f.run_id=m.id AND f.run_version_revision=m.version_revision WHERE c.project_id=? AND c.pull_number=? ORDER BY f.run_id,f.position LIMIT 10001`, project, pull); err != nil {
			return "", err
		}
	}
	dependencies, err := automaticDependencies(ctx, tx, project, pull)
	if err != nil {
		return "", err
	}
	h.Write([]byte(encoded(dependencies)))

	return hex.EncodeToString(h.Sum(nil)), nil
}

// Only a source-owned retained parent chain supplies dependency identity.
// These local facts can make a distinct intent eligible; they never substitute
// for the selection worker's fresh source or final predecessor guard.
func automaticDependencies(ctx context.Context, tx *sql.Tx, project string, pull int64) ([]any, error) {
	out := []any{}
	frame, found, err := readFrontFrame(ctx, tx, project, "hosted")
	if err != nil || !found {
		return out, err
	}
	byNumber := make(map[int64]frontPull, len(frame.prefix))
	for _, p := range frame.prefix {
		byNumber[p.Number] = p
	}
	current, found := byNumber[pull]
	if !found {
		return out, nil
	}
	seen := map[int64]bool{pull: true}
	for parent := current.ParentPullNumber; parent != 0; {
		if seen[parent] || len(seen) > 100 {
			return nil, schedulingUnavailable()
		}
		seen[parent] = true
		p, exists := byNumber[parent]
		if !exists {
			out = append(out, []any{parent, "source_prefix_incomplete"})
			break
		}
		progress, err := channelProgress(ctx, tx, project, frame.repository, parent, "hosted")
		if err != nil {
			return nil, err
		}
		control, err := readRoundRecord(ctx, tx, project, parent, "hosted")
		if err != nil {
			return nil, err
		}
		obligations, err := sourceObligationCount(ctx, tx, project, parent)
		if err != nil {
			return nil, err
		}
		routes, err := pendingIncomingRoutes(ctx, tx, project, parent)
		if err != nil {
			return nil, err
		}
		var completed int64
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM review_comparison_proofs WHERE project_id=? AND pull_number=? AND channel='hosted' AND state='ready'`, project, parent).Scan(&completed); err != nil {
			return nil, err
		}
		out = append(out, []any{parent, p.HeadSHA, p.ParentPullNumber, p.ParentSHA, progress, control.Configured, control.Epoch, control.grantRevision, control.Closed, control.Used, control.Dry, control.Tapered, completed, obligations, routes})
		parent = p.ParentPullNumber
	}
	return out, nil
}

// Planning can move on to a distinct unattempted PR without treating an
// operation's state transition as a reason to recreate that operation. A
// selected PR is suppressed by its own semantic basis, independently of peers.
func automaticPlanningBasis(ctx context.Context, tx *sql.Tx, project string, c Configuration, ignoreSelection string) (string, bool, error) {
	limit, err := automaticHostedQueuedPrefix(ctx, tx, project, c)
	if err != nil {
		return "", false, err
	}
	values := make([]any, 0, len(c.Queue))
	remaining := []int64{}
	for position, pull := range c.Queue {
		basis, err := automaticPullBasis(ctx, tx, project, pull)
		if err != nil {
			return "", false, err
		}
		values = append(values, []any{pull, basis})
		if position >= limit {
			continue
		}
		p, err := readPullScheduling(ctx, tx, project, pull)
		if err != nil {
			return "", false, err
		}
		if p.Paused {
			continue
		}
		var seen bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_automatic_selections WHERE project_id=? AND selection_id<>? AND pull_number=? AND pull_basis_sha256=?)`, project, ignoreSelection, pull, basis).Scan(&seen); err != nil {
			return "", false, err
		}
		if !seen {
			remaining = append(remaining, pull)
		}
	}
	sum := sha256.Sum256([]byte(encoded([]any{values, remaining})))
	return hex.EncodeToString(sum[:]), len(remaining) > 0, nil
}

// A waiting request keeps its identity and deadline. Automatic selection may
// reconsider only earlier PRs, rather than duplicate that request or prefetch
// farther successors merely because the request is not executing yet.
func automaticHostedQueuedPrefix(ctx context.Context, tx *sql.Tx, project string, c Configuration) (int, error) {
	queue, err := json.Marshal(c.Queue)
	if err != nil {
		return 0, err
	}
	var limit int
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(q.key),?) FROM review_operations o JOIN json_each(?) q ON q.value=o.pull_number WHERE o.project_id=? AND o.channel='hosted' AND o.state='queued'`, len(c.Queue), string(queue), project).Scan(&limit)
	return limit, err
}

func (e *Engine) automaticPullSuppressed(ctx context.Context, tx *sql.Tx, r Run, pull int64) (bool, error) {
	var automatic bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_automatic_selections WHERE selection_id=? AND project_id=?)`, r.ID, r.ProjectID).Scan(&automatic); err != nil {
		return false, err
	}
	if !automatic {
		return false, nil
	}
	c, _, err := readConfiguration(ctx, tx, r.ProjectID)
	if err != nil {
		return false, err
	}
	limit, err := automaticHostedQueuedPrefix(ctx, tx, r.ProjectID, c)
	if err != nil {
		return false, err
	}
	if position := slices.Index(c.Queue, pull); position < 0 || position >= limit {
		return true, nil
	}
	basis, err := automaticPullBasis(ctx, tx, r.ProjectID, pull)
	if err != nil {
		return false, err
	}
	var seen bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_automatic_selections WHERE project_id=? AND selection_id<>? AND pull_number=? AND pull_basis_sha256=?)`, r.ProjectID, r.ID, pull, basis).Scan(&seen)
	return seen, err
}
func (e *Engine) settleAutomaticBasis(ctx context.Context, tx *sql.Tx, r Run, pull int64) error {
	var automatic bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_automatic_selections WHERE selection_id=? AND project_id=?)`, r.ID, r.ProjectID).Scan(&automatic); err != nil {
		return err
	}
	if !automatic {
		return nil
	}
	c, _, err := readConfiguration(ctx, tx, r.ProjectID)
	if err != nil {
		return err
	}
	basis, _, err := automaticPlanningBasis(ctx, tx, r.ProjectID, c, r.ID)
	if err != nil {
		return err
	}
	pullBasis := ""
	if pull > 0 {
		pullBasis, err = automaticPullBasis(ctx, tx, r.ProjectID, pull)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE review_automatic_selections SET settled_basis_sha256=?,pull_number=?,pull_basis_sha256=? WHERE selection_id=? AND project_id=?`, basis, pull, pullBasis, r.ID, r.ProjectID)
	return err
}

// One bounded local coordinator pass. Existing selection workers own every
// provider read and fresh source/admission check; this method creates no attempt.
func (e *Engine) generateAutomaticHosted(ctx context.Context, readers map[string]TargetReader) (bool, error) {
	var projects []string
	if err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT project_id FROM review_scheduling WHERE automatic_hosted=1 ORDER BY (project_id<=?),project_id LIMIT 100`, e.automaticCursor)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			if err = rows.Scan(&p); err != nil {
				return err
			}
			projects = append(projects, p)
		}
		return rows.Err()
	}); err != nil {
		return false, err
	}
	for _, project := range projects {
		e.automaticCursor = project
		worked := false
		err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			var restored bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metadata WHERE (key='restore:provider_state' AND value='held') OR (key='restore:state' AND value='inactive'))`).Scan(&restored); err != nil {
				return err
			}
			if restored {
				return nil
			}
			s, err := readScheduling(ctx, tx, project)
			if err != nil {
				return err
			}
			if !s.AutomaticHosted {
				return nil
			}
			scope, err := e.store.ProjectAutomationScope(ctx, tx, project, s.actor, s.AuthorizingWorkerID, s.credential)
			if err != nil {
				var failure *core.Error
				if errors.As(err, &failure) && failure.Status >= 400 && failure.Status < 500 {
					return nil
				}
				return err
			}
			c, _, err := readConfiguration(ctx, tx, project)
			if err != nil {
				var failure *core.Error
				if errors.As(err, &failure) && failure.Status == 409 {
					return nil
				}
				return err
			}
			if c.Paused || !slices.Contains(c.Channels, "hosted") || e.validateConfiguration(c) != nil || readers[c.CapabilityID] == nil || len(c.Queue) == 0 {
				return nil
			}
			unpaused := false
			for _, pull := range c.Queue {
				p, err := readPullScheduling(ctx, tx, project, pull)
				if err != nil {
					return err
				}
				unpaused = unpaused || !p.Paused
			}
			if !unpaused {
				return nil
			}
			policy, err := readPolicy(ctx, tx, project)
			if err != nil {
				var failure *core.Error
				if errors.As(err, &failure) && failure.Code == "reviews_policy_unconfigured" {
					return nil
				}
				return err
			}
			if policy.Revision < 1 {
				return nil
			}
			var busy bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_selections WHERE project_id=? AND channel='hosted' AND state IN('queued','selecting')) OR EXISTS(SELECT 1 FROM review_operations WHERE project_id=? AND channel='hosted' AND state IN('preparing','posting','active','uncertain'))`, project, project).Scan(&busy); err != nil {
				return err
			}
			if busy {
				return nil
			}
			basis, candidates, err := automaticPlanningBasis(ctx, tx, project, c, "")
			if err != nil {
				return err
			}
			if !candidates {
				return nil
			}
			var seen bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_automatic_selections WHERE project_id=? AND (basis_sha256=? OR settled_basis_sha256=?))`, project, basis, basis).Scan(&seen); err != nil {
				return err
			}
			if seen {
				return nil
			}
			// Use the same acceptance implementation as explicit Run, in this exact
			// reauthorized snapshot, without manufacturing a credential or HTTP request.
			receipt, err := e.enqueueRunTx(ctx, tx, scope, RunRequest{Channel: "hosted"})
			if err != nil {
				var failure *core.Error
				if errors.As(err, &failure) && (failure.Status == 409 || failure.Code == "overloaded") {
					return nil
				}
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO review_automatic_selections(selection_id,project_id,scheduling_revision,basis_sha256,actor_id,worker_id,credential_id) VALUES(?,?,?,?,?,?,?)`, receipt.ID, project, s.Revision, basis, s.actor, s.AuthorizingWorkerID, s.credential)
			if err == nil {
				worked = true
			}
			return err
		})
		if err != nil {
			return false, err
		}
		if worked {
			return true, nil
		}
	}
	return false, nil
}

func (e *Engine) coordinateAutomaticHosted(ctx context.Context, readers map[string]TargetReader) (bool, error) {
	e.automaticMu.Lock()
	defer e.automaticMu.Unlock()
	now := time.Now()
	if now.Before(e.automaticNext) {
		return false, nil
	}
	e.automaticNext = now.Add(5 * time.Second)
	worked, err := e.generateAutomaticHosted(ctx, readers)
	var failure *core.Error
	if errors.As(err, &failure) && failure.Code == "overloaded" {
		return false, nil
	}
	return worked, err
}
