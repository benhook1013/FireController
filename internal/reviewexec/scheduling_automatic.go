package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

var errAutomaticParentCycle = errors.New("automatic scheduling retained parent cycle")

// Tag only validated project-local retained-data refusals. Public status codes
// deliberately remain unchanged; storage_unavailable alone is not a safe reason
// to suppress a failed query, transaction, or shared storage operation.
type automaticProjectStateError struct{ error }

func (e *automaticProjectStateError) Unwrap() error { return e.error }

func automaticProjectStateFailure(code, message string, status int) error {
	return &automaticProjectStateError{core.Fail(code, message, status)}
}

func automaticProjectRefused(err error) bool {
	var local *automaticProjectStateError
	return errors.As(err, &local)
}

func automaticDenied() error {
	return core.Fail("automation_authorization_changed", "Automatic review scheduling is disabled or its original authorizer is no longer authorized.", 403)
}
func (e *Engine) requireAutomaticRun(ctx context.Context, tx *sql.Tx, r Run) error {
	var retainedRevision any
	var actor, worker, credential string
	err := tx.QueryRowContext(ctx, `SELECT scheduling_revision,actor_id,worker_id,credential_id FROM review_automatic_selections WHERE project_id=? AND selection_id=?`, r.ProjectID, r.ID).Scan(&retainedRevision, &actor, &worker, &credential)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Preserve query failures while classifying malformed retained SQLite values.
	revision, validRevision := retainedRevision.(int64)
	if !validRevision || revision < 1 {
		return &automaticProjectStateError{schedulingUnavailable()}
	}
	s, err := readScheduling(ctx, tx, r.ProjectID)
	if err != nil {
		return err
	}
	if revision > s.Revision || !automaticChannelEnabled(s, r.Channel) || r.ActorID != actor || r.WorkerID != worker || !validChannel(r.Channel) {
		return automaticDenied()
	}
	if s.actor != actor || s.AuthorizingWorkerID != worker || s.credential != credential {
		continuous, err := retainedAutomaticAuthorization(ctx, tx, r, revision, actor, s)
		if err != nil {
			return err
		}
		if !continuous {
			return automaticDenied()
		}
	} else if revision != s.Revision {
		continuous, err := automaticChannelContinuous(ctx, tx, r, revision, actor, s)
		if err != nil {
			return err
		}
		if !continuous {
			return automaticDenied()
		}
	}
	_, err = e.store.ProjectAutomationScope(ctx, tx, r.ProjectID, actor, worker, credential)
	var failure *core.Error
	if errors.As(err, &failure) && failure.Status >= 400 && failure.Status < 500 {
		return automaticDenied()
	}
	if err != nil {
		return err
	}
	c, err := continuationForSelection(ctx, tx, r.ProjectID, r.ID)
	if err != nil || c == nil {
		return err
	}
	return e.validateHostedContinuation(ctx, tx, c)
}
func automaticOperationSelection(ctx context.Context, tx *sql.Tx, o Operation) (string, error) {
	lineage, lineageErr := readIntentLineage(ctx, tx, o.ProjectID, o.ID)
	if lineageErr != nil {
		return "", lineageErr
	}
	if lineage != nil {
		o.ID = lineage.RootOperationID
	}
	var selection string
	err := tx.QueryRowContext(ctx, `SELECT s.id FROM review_selections s JOIN review_automatic_selections a ON a.selection_id=s.id AND a.project_id=s.project_id WHERE s.project_id=? AND s.operation_id=?`, o.ProjectID, o.ID).Scan(&selection)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return selection, err
}

func (e *Engine) requireAutomaticOperation(ctx context.Context, tx *sql.Tx, o Operation) error {
	selection, err := automaticOperationSelection(ctx, tx, o)
	if err != nil || selection == "" {
		return err
	}
	r, err := readRun(ctx, tx, o.ProjectID, selection)
	if err != nil {
		return err
	}
	originalID := o.ID
	lineage, lineageErr := readIntentLineage(ctx, tx, o.ProjectID, o.ID)
	if lineageErr != nil {
		return lineageErr
	}
	if lineage != nil {
		originalID = lineage.RootOperationID
	}
	if r.OperationID != originalID || r.ActorID != o.ActorID || r.WorkerID != o.WorkerID || r.Channel != o.Channel {
		return &automaticProjectStateError{schedulingUnavailable()}
	}
	if err := e.requireAutomaticRun(ctx, tx, r); err != nil {
		return err
	}
	c, err := continuationForSelection(ctx, tx, r.ProjectID, r.ID)
	if err != nil || c == nil {
		return err
	}
	if c.Child != o.ID {
		return sourceHold("hosted_continuation_changed")
	}
	if o.AttemptID != "" {
		a, err := readAttempt(ctx, tx, o.ProjectID, o.AttemptID)
		if err != nil {
			return err
		}
		if a.Anchor != nil && !hostedAllowanceSameSource(c.Anchor, *a.Anchor) {
			return sourceHold("source_changed")
		}
	}
	return nil
}

const automaticCurrentRunsBasisQuery = `SELECT v.id,v.channel,v.head_sha,v.complete,v.finalized,v.found,v.accepted,v.routed,v.fixed FROM review_current_runs c JOIN review_run_memberships m ON m.project_id=c.project_id AND m.id=c.id AND m.start_revision=c.start_revision JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision WHERE c.project_id=? AND c.pull_number=? AND v.channel='hosted' AND v.outcome='completed' ORDER BY v.id LIMIT 10001`
const automaticCurrentFindingsBasisQuery = `SELECT f.run_id,f.source_finding_id,f.disposition,f.fix_sha,f.proof_note,f.route_json FROM review_current_runs c JOIN review_run_memberships m ON m.project_id=c.project_id AND m.id=c.id AND m.start_revision=c.start_revision JOIN review_finding_versions f ON f.project_id=m.project_id AND f.run_id=m.id AND f.run_version_revision=m.version_revision WHERE c.project_id=? AND c.pull_number=? ORDER BY f.run_id,f.position LIMIT 10001`

// Query text remains part of the retained intent digest. Pin only physical join
// order for current-reference reads; never replace the digest input with hints.
func automaticBasisQuery(query string, pinJoins bool) string {
	if pinJoins {
		return strings.ReplaceAll(query, " JOIN ", " CROSS JOIN ")
	}
	return query
}

// The semantic digest excludes projection revisions, observation timestamps,
// attempt states and selection/operation transitions. Failed/cancelled requests
// therefore cannot create their own next automatic request. Only original source
// facts and deliberate controls, completed results and finding work change it.
func automaticPullBasis(ctx context.Context, tx *sql.Tx, project string, pull int64) (string, error) {
	return automaticPullBasisChannel(ctx, tx, project, pull, "hosted")
}

func automaticChannelBasisRevision(ctx context.Context, tx *sql.Tx, project, channel string) (int64, error) {
	s, err := readScheduling(ctx, tx, project)
	if err != nil {
		return 0, err
	}
	hostedRevision, cliRevision, err := schedulingBasisRevisions(ctx, tx, s)
	if channel == "cli" {
		return cliRevision, err
	}
	return hostedRevision, err
}

func automaticPullBasisChannel(ctx context.Context, tx *sql.Tx, project string, pull int64, channel string) (string, error) {
	revision, err := automaticChannelBasisRevision(ctx, tx, project, channel)
	if err != nil {
		return "", err
	}
	return automaticPullBasisAtRevision(ctx, tx, project, pull, channel, revision)
}

func automaticPullBasisAtRevision(ctx context.Context, tx *sql.Tx, project string, pull int64, channel string, basisRevision int64) (string, error) {
	h := sha256.New()
	appendRows := func(query string, pinJoins bool, args ...any) error {
		if channel == "cli" {
			query = strings.ReplaceAll(query, "revision,automatic_hosted", "revision,automatic_cli")
		}
		h.Write([]byte(query))
		h.Write([]byte{0})
		rows, err := tx.QueryContext(ctx, automaticBasisQuery(query, pinJoins), args...)
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
				return automaticProjectStateFailure("overloaded", "Automatic scheduling needs a narrower retained review scope.", 503)
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
			// Keep the original query label and JSON framing. Only the
			// channel's semantic revision differs after a relevant write.
			if query == `SELECT revision,automatic_`+channel+` FROM review_scheduling WHERE project_id=?` {
				values[0] = basisRevision
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
		{`SELECT epoch,grant_revision,closed,minimum,maximum,maximum_unbounded,used,dry,tapered FROM review_round_controls WHERE project_id=? AND pull_number=? AND channel='` + channel + `'`, []any{project, pull}},
		{`SELECT a.evidence_id,a.revision FROM review_adjudication_state a JOIN review_execution_evidence e ON e.id=a.evidence_id AND e.project_id=a.project_id JOIN review_operations o ON o.id=e.operation_id AND o.project_id=e.project_id WHERE a.project_id=? AND o.pull_number=? AND (o.channel='` + channel + `' OR json_array_length(json_extract(e.body_json,'$.capture.review.Findings'))>0) ORDER BY a.evidence_id LIMIT 10001`, []any{project, pull}},
		{`SELECT id,revision,state,disposition,has_fix FROM review_finding_routes WHERE project_id=? AND (source_pull_number=? OR target_pull_number=?) ORDER BY id LIMIT 10001`, []any{project, pull, pull}},
		{`SELECT o.id,o.revision,o.status FROM review_adoption_targets t JOIN review_adoption_obligations o ON o.id=t.obligation_id AND o.project_id=t.project_id WHERE t.project_id=? AND t.pull_number=? ORDER BY o.id LIMIT 10001`, []any{project, pull}},
	}
	for _, q := range queries {
		if err := appendRows(q.sql, false, q.args...); err != nil {
			return "", err
		}
	}
	var hasWorkspace bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='review_current_pulls' AND type='table')`).Scan(&hasWorkspace); err != nil {
		return "", err
	}
	if hasWorkspace {
		if err := appendRows(`SELECT p.state,p.head_sha,p.base_ref,p.base_sha,json_extract(p.metadata_json,'$.is_draft') FROM review_current_pulls c JOIN review_pull_memberships m ON m.project_id=c.project_id AND m.number=c.number AND m.start_revision=c.start_revision JOIN review_pull_versions p ON p.project_id=m.project_id AND p.number=m.number AND p.version_revision=m.version_revision WHERE c.project_id=? AND c.number=?`, false, project, pull); err != nil {
			return "", err
		}
		if err := appendRows(strings.ReplaceAll(automaticCurrentRunsBasisQuery, "'hosted'", "'"+channel+"'"), true, project, pull); err != nil {
			return "", err
		}
		if err := appendRows(automaticCurrentFindingsBasisQuery, true, project, pull); err != nil {
			return "", err
		}
	}
	dependencies, err := automaticDependenciesChannel(ctx, tx, project, pull, channel)
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
	return automaticDependenciesChannel(ctx, tx, project, pull, "hosted")
}

func automaticDependenciesChannel(ctx context.Context, tx *sql.Tx, project string, pull int64, channel string) ([]any, error) {
	out := []any{}
	frame, found, err := readFrontFrame(ctx, tx, project, channel)
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
			return nil, errors.Join(errAutomaticParentCycle, schedulingUnavailable())
		}
		seen[parent] = true
		p, exists := byNumber[parent]
		if !exists {
			out = append(out, []any{parent, "source_prefix_incomplete"})
			break
		}
		progress, err := channelProgress(ctx, tx, project, frame.repository, parent, channel)
		if err != nil {
			return nil, err
		}
		control, err := readRoundRecord(ctx, tx, project, parent, channel)
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
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM review_comparison_proofs WHERE project_id=? AND pull_number=? AND channel='`+channel+`' AND state='ready'`, project, parent).Scan(&completed); err != nil {
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
	return automaticPlanningBasisChannel(ctx, tx, project, c, ignoreSelection, "hosted")
}

func automaticPlanningBasisChannel(ctx context.Context, tx *sql.Tx, project string, c Configuration, ignoreSelection string, channel string) (string, bool, error) {
	order, err := reviews.OrderedExecutionQueue(ctx, tx, project, c.Queue)
	if err != nil {
		return "", false, err
	}
	c.Queue = order
	limit, err := automaticHostedQueuedPrefixChannel(ctx, tx, project, c, channel)
	if err != nil {
		return "", false, err
	}
	// The channel's authorization history is fixed within this transaction.
	// Verify it once per plan rather than scanning the same journal per PR.
	revision, err := automaticChannelBasisRevision(ctx, tx, project, channel)
	if err != nil {
		return "", false, err
	}
	// Head identities are explicitly retained-source facts in this planning phase.
	// This does not perform source IO or claim admission freshness.
	heads := map[int64]string{}
	if frame, found, readErr := readFrontFrame(ctx, tx, project, channel); readErr == nil && found {
		for _, p := range frame.plans() {
			heads[p.Pull.Number] = p.Pull.HeadSHA
		}
	}
	values := make([]any, 0, len(c.Queue))
	remaining := []int64{}
	for position, pull := range c.Queue {
		basis, err := automaticPullBasisAtRevision(ctx, tx, project, pull, channel, revision)
		if err != nil {
			return "", false, err
		}
		values = append(values, []any{pull, basis})
		traceDecision(ctx, pull, heads[pull], "candidate", "source_selection_required", 0)
		if position >= limit {
			traceDecision(ctx, pull, heads[pull], "held", "queued_prefix", 0)
			continue
		}
		p, err := readPullScheduling(ctx, tx, project, pull)
		if err != nil {
			return "", false, err
		}
		if p.Paused {
			traceDecision(ctx, pull, heads[pull], "held", "pull_scheduling_paused", 0)
			continue
		}
		pending, err := automaticHostedPendingPullChannel(ctx, tx, project, pull, channel)
		if err != nil {
			return "", false, err
		}
		if pending {
			traceDecision(ctx, pull, heads[pull], "held", "operation_pending", 0)
			continue
		}
		var seen bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_automatic_selections WHERE project_id=? AND selection_id<>? AND pull_number=? AND pull_basis_sha256=?)`, project, ignoreSelection, pull, basis).Scan(&seen); err != nil {
			return "", false, err
		}
		if !seen {
			remaining = append(remaining, pull)
		} else {
			traceDecision(ctx, pull, heads[pull], "held", "automatic_intent_retained", 0)
		}
	}
	sum := sha256.Sum256([]byte(encoded([]any{values, remaining})))
	return hex.EncodeToString(sum[:]), len(remaining) > 0, nil
}

// Undispatched intents do not reserve the rest of a channel's queue. Discovery
// skips each PR's existing intent and still applies current source, dependency,
// findings, cap and authorization gates to later candidates. Actual attempt
// claims retain priority and shared provider/source cooldown serialization.
func automaticHostedQueuedPrefix(ctx context.Context, tx *sql.Tx, project string, c Configuration) (int, error) {
	return automaticHostedQueuedPrefixChannel(ctx, tx, project, c, "hosted")
}
func automaticHostedQueuedPrefixChannel(_ context.Context, _ *sql.Tx, _ string, c Configuration, channel string) (int, error) {
	if !validChannel(channel) {
		return 0, invalid()
	}
	return len(c.Queue), nil
}

func automaticHostedPendingPull(ctx context.Context, tx *sql.Tx, project string, pull int64) (bool, error) {
	return automaticHostedPendingPullChannel(ctx, tx, project, pull, "hosted")
}

func automaticHostedPendingPullChannel(ctx context.Context, tx *sql.Tx, project string, pull int64, channel string) (bool, error) {
	var pending bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_operations WHERE project_id=? AND pull_number=? AND channel='`+channel+`' AND state IN('queued','preparing','posting','active','uncertain'))`, project, pull).Scan(&pending)
	return pending, err
}

func (e *Engine) automaticPullSuppressed(ctx context.Context, tx *sql.Tx, r Run, plan *SourcePlan) (bool, error) {
	pull := plan.Pull.Number
	var automatic bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_automatic_selections WHERE selection_id=? AND project_id=?)`, r.ID, r.ProjectID).Scan(&automatic); err != nil {
		return false, err
	}
	if !automatic {
		return false, nil
	}
	if r.Channel == "cli" {
		exists, err := partialCLIComparisonExists(ctx, tx, r.ProjectID, plan.Repository.ID, pull,
			plan.Pull.HeadSHA, plan.Pull.BaseSHA, plan.Pull.BaseRef, plan.ParentSHA, plan.ParentPullNumber)
		if err != nil || exists {
			return exists, err
		}
	}
	pending, err := automaticHostedPendingPullChannel(ctx, tx, r.ProjectID, pull, r.Channel)
	if err != nil || pending {
		return pending, err
	}
	c, _, err := readConfiguration(ctx, tx, r.ProjectID)
	if err != nil {
		return false, err
	}
	c.Queue, err = reviews.OrderedExecutionQueue(ctx, tx, r.ProjectID, c.Queue)
	if err != nil {
		return false, err
	}
	limit, err := automaticHostedQueuedPrefixChannel(ctx, tx, r.ProjectID, c, r.Channel)
	if err != nil {
		return false, err
	}
	if position := slices.Index(c.Queue, pull); position < 0 || position >= limit {
		return true, nil
	}
	basis, err := automaticPullBasisChannel(ctx, tx, r.ProjectID, pull, r.Channel)
	if err != nil {
		return false, err
	}
	cRetry, retryErr := continuationForSelection(ctx, tx, r.ProjectID, r.ID)
	if retryErr != nil {
		return false, retryErr
	}
	if cRetry != nil && plan.Pull.Number == cRetry.Anchor.PullNumber {
		if err := e.validateHostedContinuation(ctx, tx, cRetry); err != nil {
			return false, err
		}
		return !continuationMatches(cRetry, plan), nil
	}
	if r.Channel == "hosted" && cRetry == nil {
		reserved, err := e.continuationOwnsPull(ctx, tx, r.ProjectID, pull)
		if err != nil || reserved {
			return reserved, err
		}
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
	if pull == 0 {
		continuation, err := continuationForSelection(ctx, tx, r.ProjectID, r.ID)
		if err != nil || continuation != nil {
			return err
		}
	}
	c, _, err := readConfiguration(ctx, tx, r.ProjectID)
	if err != nil {
		return err
	}
	basis, _, err := automaticPlanningBasisChannel(ctx, tx, r.ProjectID, c, r.ID, r.Channel)
	if err != nil {
		return err
	}
	pullBasis := ""
	if pull > 0 {
		pullBasis, err = automaticPullBasisChannel(ctx, tx, r.ProjectID, pull, r.Channel)
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
		worked, err := e.generateAutomaticProject(ctx, project, "hosted", readers)
		if automaticProjectRefused(err) {
			continue
		}
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
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	now := time.Now()
	if now.Before(e.automaticNext) {
		return false, nil
	}
	e.automaticNext = now.Add(5 * time.Second)
	worked, hostedErr := e.generateAutomaticHosted(ctx, readers)
	if ctx.Err() != nil {
		return worked, ctx.Err()
	}
	cliWorked, cliErr := e.generateAutomaticCLI(ctx, readers)
	if ctx.Err() != nil {
		return worked || cliWorked, ctx.Err()
	}
	// A transient admission/deadline refusal in one channel must not suppress
	// the other channel or hide its fatal storage error.
	transient := func(err error) error {
		var failure *core.Error
		if errors.As(err, &failure) && failure.Code == "overloaded" {
			return nil
		}
		return err
	}
	return worked || cliWorked, errors.Join(transient(hostedErr), transient(cliErr))
}

func automaticChannelEnabled(s schedulingRecord, channel string) bool {
	return channel == "hosted" && s.AutomaticHosted || channel == "cli" && s.AutomaticCLI
}

// The read preflight can only nominate work. Repeat the entire plan under the
// serialized writer and compare its identity before using ordinary acceptance
// or retry CAS. Idle and permanently suppressed projects never take the writer.
type automaticProjectPlan struct {
	scheduling     schedulingRecord
	scope          core.Scope
	basis          string
	retry          *Run
	operationRetry *Operation
	continuation   *hostedContinuation
}

func (e *Engine) planAutomaticProject(ctx context.Context, tx *sql.Tx, project, channel string, readers map[string]TargetReader) (*automaticProjectPlan, error) {
	var restored bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metadata WHERE (key='restore:provider_state' AND value='held') OR (key='restore:state' AND value='inactive'))`).Scan(&restored); err != nil {
		return nil, err
	}
	if restored {
		traceReason(ctx, "provider_restore_hold")
		return nil, nil
	}
	s, err := readScheduling(ctx, tx, project)
	if !automaticChannelEnabled(s, channel) {
		traceReason(ctx, "automation_disabled")
	}
	if err != nil || !automaticChannelEnabled(s, channel) {
		return nil, err
	}
	scope, err := e.store.ProjectAutomationScope(ctx, tx, project, s.actor, s.AuthorizingWorkerID, s.credential)
	if err != nil {
		var failure *core.Error
		if errors.As(err, &failure) && failure.Status >= 400 && failure.Status < 500 {
			traceReason(ctx, "automation_authorization_unavailable")
			return nil, nil
		}
		return nil, err
	}
	c, _, err := readConfiguration(ctx, tx, project)
	if err != nil {
		var failure *core.Error
		if errors.As(err, &failure) && failure.Code == "reviews_unconfigured" {
			traceReason(ctx, "reviews_unconfigured")
			return nil, nil
		}
		return nil, err
	}
	if c.Paused || !slices.Contains(c.Channels, channel) || e.validateConfiguration(c) != nil || readers[c.CapabilityID] == nil || len(c.Queue) == 0 {
		traceReason(ctx, "configuration_unavailable")
		return nil, nil
	}
	// Discovery must not spend source reads while a proven shared negative
	// floor or an actually occupied provider lane prevents any start. A PR's
	// own Hosted cooldown is not evidence that unrelated PRs must wait.
	if ready, err := e.sourceCandidateDue(ctx, tx, Operation{}, c); err != nil {
		return nil, err
	} else if !ready {
		traceReason(ctx, "source_credential_wait")
		return nil, nil
	}
	capability := e.capabilities[c.CapabilityID]
	if channel == "cli" {
		due, err := cliAccountRetryDeadline(ctx, tx, capability, e.sampleExecutionTime())
		if err != nil {
			return nil, err
		}
		if e.sampleExecutionTime().Before(due) {
			traceReason(ctx, "provider_account_wait")
			return nil, nil
		}
	}
	var providerBusy bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_execution_reservations r JOIN projects p ON p.id=? WHERE r.provider=? AND r.account=? AND r.action_class=? AND (?='cli' OR r.repository_id=p.github_repository_id))`, project, capability.Provider, capability.Account, channel, channel).Scan(&providerBusy); err != nil {
		return nil, err
	}
	if providerBusy {
		traceReason(ctx, "provider_lane_busy")
		return nil, nil
	}
	c.Queue, err = reviews.OrderedExecutionQueue(ctx, tx, project, c.Queue)
	if err != nil {
		return nil, err
	}
	unpaused := false
	for _, pull := range c.Queue {
		p, err := readPullScheduling(ctx, tx, project, pull)
		if err != nil {
			return nil, err
		}
		unpaused = unpaused || !p.Paused
	}
	if !unpaused {
		traceReason(ctx, "all_prs_paused")
		return nil, nil
	}
	policy, err := readPolicy(ctx, tx, project)
	if err != nil {
		var failure *core.Error
		if errors.As(err, &failure) && failure.Code == "reviews_policy_unconfigured" {
			traceReason(ctx, "reviews_policy_unconfigured")
			return nil, nil
		}
		return nil, err
	}
	if policy.Revision < 1 {
		traceReason(ctx, "reviews_policy_unconfigured")
		return nil, nil
	}
	var busy bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_selections WHERE project_id=? AND channel=? AND state IN('queued','selecting')) OR EXISTS(SELECT 1 FROM review_operations WHERE project_id=? AND channel=? AND state IN('preparing','posting','active','uncertain'))`, project, channel, project, channel).Scan(&busy); err != nil {
		return nil, err
	}
	if busy {
		traceReason(ctx, "lane_busy")
		return nil, nil
	}
	if channel == "cli" {
		retry, err := e.automaticTransientCLIRetry(ctx, tx, project, c)
		if err != nil {
			return nil, err
		}
		if retry != nil {
			return &automaticProjectPlan{scheduling: s, scope: scope, operationRetry: retry}, nil
		}
	}
	// Ordinary eligible work keeps priority. A receipt that meets a transient
	// earlier front retains its same selection for a delayed guarded recheck.
	continuationPlan := func() (*automaticProjectPlan, error) {
		if channel != "hosted" {
			return nil, nil
		}
		cRetry, err := e.pendingHostedContinuation(ctx, tx, project, c)
		if err != nil || cRetry == nil {
			return nil, err
		}
		p := &automaticProjectPlan{scheduling: s, scope: scope, basis: continuationBasis(cRetry), continuation: cRetry}
		if cRetry.Selection != "" {
			r, err := readRun(ctx, tx, project, cRetry.Selection)
			if err != nil {
				return nil, err
			}
			p.retry = &r
		}
		return p, nil
	}
	continuation, err := continuationPlan()
	if err != nil {
		return nil, err
	}
	if continuation != nil && continuation.retry == nil {
		return continuation, nil
	}
	if continuation != nil && continuation.retry != nil {
		prefix := c
		position := slices.Index(c.Queue, continuation.continuation.Anchor.PullNumber)
		if position < 0 {
			return nil, nil
		}
		prefix.Queue = c.Queue[:position]
		_, earlier, err := automaticPlanningBasisChannel(ctx, tx, project, prefix, "", channel)
		if err != nil {
			return nil, err
		}
		if !earlier {
			return continuation, nil
		}
	}
	basis, candidates, err := automaticPlanningBasisChannel(ctx, tx, project, c, "", channel)
	if err != nil {
		return nil, err
	}
	if !candidates {
		return continuationPlan()
	}
	// A successful selection consumes only its chosen PR. Its post-selection
	// digest must not suppress unattempted peers now that queued intents do not
	// truncate planning. An unproductive pass still deduplicates its whole basis.
	var seen bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_automatic_selections WHERE project_id=? AND (basis_sha256=? OR settled_basis_sha256=?))`, project, basis, basis).Scan(&seen); err != nil {
		return nil, err
	}
	// A selected pass examined only through its chosen PR. Preserve dedupe for
	// its held prefix, but allow unattempted candidates beyond that frontier.
	if seen {
		var selected int64
		if err = tx.QueryRowContext(ctx, `SELECT pull_number FROM review_automatic_selections WHERE project_id=? AND (basis_sha256=? OR settled_basis_sha256=?) ORDER BY rowid DESC LIMIT 1`, project, basis, basis).Scan(&selected); err != nil {
			return nil, err
		}
		if position := slices.Index(c.Queue, selected); selected > 0 && position >= 0 && position+1 < len(c.Queue) {
			tail := c
			tail.Queue = c.Queue[position+1:]
			_, unseen, err := automaticPlanningBasisChannel(ctx, tx, project, tail, "", channel)
			if err != nil {
				return nil, err
			}
			if unseen {
				seen = false
			}
		}
	}
	plan := &automaticProjectPlan{scheduling: s, scope: scope, basis: basis}
	if seen {
		plan.retry, err = e.automaticSourceRetryCandidate(ctx, tx, project, basis, s.Revision, time.Now(), channel)
		if err != nil {
			return nil, err
		}
		if plan.retry == nil {
			return continuationPlan()
		}
	}
	return plan, nil
}

func (e *Engine) generateAutomaticProject(ctx context.Context, project, channel string, readers map[string]TargetReader) (worked bool, resultErr error) {
	trace := &decisionTrace{}
	ctx = context.WithValue(ctx, decisionTraceKey{}, trace)
	defer func() {
		if ctx.Err() != nil {
			return
		}
		outcome, reason := "no_new_intent", trace.reason
		if worked {
			outcome = "scheduler_action_committed"
		}
		if resultErr != nil {
			outcome = "error"
			reason = runReason(resultErr)
		}
		// A busy writer must not turn an idle scheduler pass into a work-stopping wait.
		telemetryCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		defer cancel()
		// Observation failure never authorizes or cancels scheduler work.
		err := e.store.BackgroundWrite(telemetryCtx, func(ctx context.Context, tx *sql.Tx) error {
			return writeSelectionDecision(ctx, tx, project, channel, SelectionDecision{Phase: "automatic_plan", Outcome: outcome, Reason: reason, Candidates: trace.candidates}, time.Now())
		})
		if err != nil {
			e.decisionOmissions.add(project, channel)
			slog.WarnContext(ctx, "scheduler decision history unavailable", "project_id", project, "channel", channel)
		}
	}()

	var candidate *automaticProjectPlan
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		candidate, err = e.planAutomaticProject(ctx, tx, project, channel, readers)
		return err
	})
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil || candidate == nil {
		return false, err
	}
	return e.acceptAutomaticProject(ctx, project, channel, readers, candidate)
}

func (e *Engine) acceptAutomaticProject(ctx context.Context, project, channel string, readers map[string]TargetReader, candidate *automaticProjectPlan) (bool, error) {
	worked := false
	var requeued string
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := e.planAutomaticProject(ctx, tx, project, channel, readers)
		if err != nil || current == nil {
			return err
		}
		if current.basis != candidate.basis || current.scheduling.Revision != candidate.scheduling.Revision || (current.retry == nil) != (candidate.retry == nil) ||
			(current.operationRetry == nil) != (candidate.operationRetry == nil) || (current.continuation == nil) != (candidate.continuation == nil) {
			return nil
		}
		if current.operationRetry != nil {
			if current.operationRetry.ID != candidate.operationRetry.ID || current.operationRetry.Revision != candidate.operationRetry.Revision {
				return nil
			}
			err = e.requeueTransientCLISource(ctx, tx, *current.operationRetry)
			requeued = current.operationRetry.ID
		} else if current.retry != nil {
			if current.retry.ID != candidate.retry.ID || current.retry.Revision != candidate.retry.Revision {
				return nil
			}
			err = updateRunAt(ctx, tx, *current.retry, "queued", "", "", time.Now())
		} else {
			// Same ordinary acceptance path, fully reauthorized in this snapshot.
			request := RunRequest{Channel: channel}
			if current.continuation != nil {
				request.ExpectedPullNumber = &current.continuation.Anchor.PullNumber
			}
			receipt, enqueueErr := e.enqueueRunTx(ctx, tx, current.scope, request)
			if enqueueErr != nil {
				var failure *core.Error
				if errors.As(enqueueErr, &failure) && (failure.Status == 409 || failure.Code == "overloaded") {
					return nil
				}
				return enqueueErr
			}
			s := current.scheduling
			_, err = tx.ExecContext(ctx, `INSERT INTO review_automatic_selections(selection_id,project_id,scheduling_revision,basis_sha256,actor_id,worker_id,credential_id) VALUES(?,?,?,?,?,?,?)`, receipt.ID, project, s.Revision, current.basis, s.actor, s.AuthorizingWorkerID, s.credential)
			if err == nil && current.continuation != nil {
				var result sql.Result
				result, err = tx.ExecContext(ctx, `UPDATE review_hosted_continuations SET child_selection=? WHERE root_attempt=? AND child_selection IS NULL`, receipt.ID, current.continuation.Root)
				if err == nil {
					var n int64
					n, err = result.RowsAffected()
					if err == nil && n != 1 {
						return conflict()
					}
				}
			}
		}
		worked = err == nil
		return err
	})
	if err == nil && requeued != "" {
		e.notifyOperation(requeued)
		select {
		case e.wake <- struct{}{}:
		default:
		}
	}
	if ctx.Err() != nil {
		return worked && err == nil, ctx.Err()
	}
	return worked && err == nil, err
}
