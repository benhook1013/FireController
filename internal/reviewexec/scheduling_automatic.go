package reviewexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
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
	attribution, err := automaticSelectionBinding(ctx, tx, r)
	if err != nil || attribution.revision == 0 {
		return err
	}
	revision, credential := attribution.revision, attribution.credential
	actor, worker := r.ActorID, r.WorkerID
	s, err := readScheduling(ctx, tx, r.ProjectID)
	if err != nil {
		return err
	}
	if revision > s.Revision || !automaticChannelEnabled(s, r.Channel) || !validChannel(r.Channel) {
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
	} else {
		// A same-revision retry still requires the exact canonical event
		// behind its enabled control. Use the same basis validation as
		// a scheduling edit; a retained projection alone is not proof.
		if _, _, err := schedulingBasisRevisions(ctx, tx, s); err != nil {
			return err
		}
	}
	_, err = e.store.ProjectAutomationScope(ctx, tx, r.ProjectID, actor, worker, credential)
	var failure *core.Error
	if errors.As(err, &failure) && failure.Status >= 400 && failure.Status < 500 {
		return automaticDenied()
	}
	return err
}

// operationSelectionBindingStateError identifies a malformed, directly
// observed selection-to-operation link, not a missing or unreadable history.
type operationSelectionBindingStateError struct{ error }

func (e *operationSelectionBindingStateError) Unwrap() error { return e.error }

func (e *Engine) requireAutomaticOperation(ctx context.Context, tx *sql.Tx, o Operation) error {
	if err := requireUnquarantinedSelectionLink(ctx, tx, o.ProjectID, o.ID); err != nil {
		return err
	}
	var project, selection any
	err := tx.QueryRowContext(ctx, `SELECT project_id,id FROM review_selections WHERE operation_id=?`, o.ID).Scan(&project, &selection)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	id, validID := selection.(string)
	if project != o.ProjectID || !validID || !validRunID(id) {
		return &operationSelectionBindingStateError{schedulingUnavailable()}
	}
	r, err := readRun(ctx, tx, o.ProjectID, id)
	if err != nil {
		return err
	}
	if r.OperationID != o.ID || r.ActorID != o.ActorID || r.WorkerID != o.WorkerID || r.Channel != o.Channel {
		return &operationSelectionBindingStateError{schedulingUnavailable()}
	}
	return e.requireAutomaticRun(ctx, tx, r)
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

type automaticBasisRead struct {
	prefix       []byte
	revision     int64
	workspace    bool
	dependencies automaticDependencyRead
}

// The channel semantic revision is independent of unrelated control edits.
func automaticChannelBasisRevision(ctx context.Context, tx *sql.Tx, project, channel string) (int64, error) {
	s, err := readScheduling(ctx, tx, project)
	if err != nil {
		return 0, err
	}
	hosted, cli, err := schedulingBasisRevisions(ctx, tx, s)
	if channel == "cli" {
		return cli, err
	}
	return hosted, err
}

// A planning pass owns one read snapshot. These shared facts never escape it.
func readAutomaticBasis(ctx context.Context, tx *sql.Tx, project, channel string) (automaticBasisRead, error) {
	var out automaticBasisRead
	var err error
	out.revision, err = automaticChannelBasisRevision(ctx, tx, project, channel)
	if err != nil {
		return out, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='review_current_pulls' AND type='table')`).Scan(&out.workspace); err != nil {
		return out, err
	}
	frame, found, err := readFrontFrame(ctx, tx, project, channel)
	if err != nil {
		return out, err
	}
	out.dependencies = newAutomaticDependencyRead(ctx, tx, project, channel, frame, found)
	var prefix bytes.Buffer
	appendRows := automaticBasisAppender(ctx, tx, &prefix, channel, out.revision)
	for _, query := range []string{
		`SELECT revision,automatic_hosted FROM review_scheduling WHERE project_id=?`,
		`SELECT revision FROM review_execution_config WHERE project_id=? ORDER BY revision DESC LIMIT 1`,
		`SELECT revision FROM review_policy_config WHERE project_id=? ORDER BY revision DESC LIMIT 1`,
	} {
		if err := appendRows(query, false, project); err != nil {
			return out, err
		}
	}
	out.prefix = prefix.Bytes()
	return out, nil
}

func automaticPullBasisChannel(ctx context.Context, tx *sql.Tx, project string, pull int64, channel string) (string, error) {
	shared, err := readAutomaticBasis(ctx, tx, project, channel)
	if err != nil {
		return "", err
	}
	return automaticPullBasisWithRead(ctx, tx, project, pull, channel, shared)
}

// This historical label is part of the retained digest. Do not change it
// when tightening the SQL used to obtain the same valid rows.
func automaticAdjudicationBasisLabel(channel string) string {
	return `SELECT a.evidence_id,a.revision FROM review_adjudication_state a JOIN review_execution_evidence e ON e.id=a.evidence_id AND e.project_id=a.project_id JOIN review_operations o ON o.id=e.operation_id AND o.project_id=e.project_id WHERE a.project_id=? AND o.pull_number=? AND (o.channel='` + channel + `' OR json_array_length(json_extract(e.body_json,'$.capture.review.Findings'))>0) ORDER BY a.evidence_id LIMIT 10001`
}

func automaticAdjudicationBasisSQL(channel string) string {
	// CASE keeps JSON functions away from unavailable input. A null/omitted
	// slice was historically empty, not malformed. Keep same-channel rows
	// and their contribution independent of the opposite-channel test.
	return `WITH evidence AS (
		SELECT a.evidence_id,a.revision,CASE
		  WHEN o.channel='` + channel + `' THEN 1
		  WHEN typeof(e.body_json)='text' AND length(CAST(e.body_json AS BLOB))<=` + strconv.Itoa(MaxEvidenceInputBytes+8192) + `
		  THEN CASE WHEN json_valid(e.body_json) THEN CASE
		    WHEN json_type(e.body_json,'$.capture.review.Findings')='array' THEN json_array_length(e.body_json,'$.capture.review.Findings')
		    WHEN COALESCE(json_type(e.body_json,'$.capture.review.Findings'),'null')='null' THEN 0
		  END END END AS count
		FROM review_adjudication_state a JOIN review_execution_evidence e ON e.id=a.evidence_id AND e.project_id=a.project_id
		JOIN review_operations o ON o.id=e.operation_id AND o.project_id=e.project_id
		WHERE a.project_id=? AND o.pull_number=?
	)
	SELECT CASE WHEN count IS NOT NULL THEN evidence_id END,revision FROM evidence
	WHERE count IS NULL OR count>0 ORDER BY evidence_id LIMIT 10001`
}

// Only a known unreadable pull-local adjudication cell may degrade a pause
// entry; database/query, authorization, and retained predecessor failures cannot.
type automaticPullAdjudicationUnavailable struct{ error }

func (e *automaticPullAdjudicationUnavailable) Unwrap() error { return e.error }

func automaticBasisAppender(ctx context.Context, tx *sql.Tx, h io.Writer, channel string, basisRevision int64) func(string, bool, ...any) error {
	return func(query string, pinJoins bool, args ...any) error {
		if channel == "cli" {
			query = strings.ReplaceAll(query, "revision,automatic_hosted", "revision,automatic_cli")
		}
		h.Write([]byte(query))
		h.Write([]byte{0})
		executionQuery := automaticBasisQuery(query, pinJoins)
		adjudication := query == automaticAdjudicationBasisLabel(channel)
		if adjudication {
			executionQuery = automaticAdjudicationBasisSQL(channel)
		}
		rows, err := tx.QueryContext(ctx, executionQuery, args...)
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
			if adjudication && values[0] == nil {
				return &automaticPullAdjudicationUnavailable{schedulingUnavailable()}
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
}

func automaticPullBasisWithRead(ctx context.Context, tx *sql.Tx, project string, pull int64, channel string, shared automaticBasisRead) (string, error) {
	basisRevision := shared.revision
	h := sha256.New()
	h.Write(shared.prefix)
	appendRows := automaticBasisAppender(ctx, tx, h, channel, basisRevision)
	queries := []struct {
		sql  string
		args []any
	}{
		{`SELECT revision,paused,reason FROM review_pull_scheduling WHERE project_id=? AND pull_number=?`, []any{project, pull}},
		{`SELECT epoch,grant_revision,closed,minimum,maximum,maximum_unbounded,used,dry,tapered FROM review_round_controls WHERE project_id=? AND pull_number=? AND channel='` + channel + `'`, []any{project, pull}},
		{automaticAdjudicationBasisLabel(channel), []any{project, pull}},
		{`SELECT id,revision,state,disposition,has_fix FROM review_finding_routes WHERE project_id=? AND (source_pull_number=? OR target_pull_number=?) ORDER BY id LIMIT 10001`, []any{project, pull, pull}},
		{`SELECT o.id,o.revision,o.status FROM review_adoption_targets t JOIN review_adoption_obligations o ON o.id=t.obligation_id AND o.project_id=t.project_id WHERE t.project_id=? AND t.pull_number=? ORDER BY o.id LIMIT 10001`, []any{project, pull}},
	}
	var unavailable error
	for _, q := range queries {
		if err := appendRows(q.sql, false, q.args...); err != nil {
			var local *automaticPullAdjudicationUnavailable
			if !errors.As(err, &local) {
				return "", err
			}
			unavailable = err
		}
	}
	if shared.workspace {
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
	dependencies, err := shared.dependencies.forPull(pull)
	if err != nil {
		return "", err
	}
	if unavailable != nil {
		return "", unavailable
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
	frame, found, err := readFrontFrame(ctx, tx, project, channel)
	if err != nil {
		return nil, err
	}
	return newAutomaticDependencyRead(ctx, tx, project, channel, frame, found).forPull(pull)
}

// A dependency read owns one planning snapshot and one project's channel.
// Keep traversal guards per descendant, but read each ancestor's immutable
// transaction facts once. A writer replan always constructs a fresh read.
type automaticDependencyRead struct {
	byNumber map[int64]frontPull
	facts    map[int64][]any
	load     func(frontPull) ([]any, error)
}

func newAutomaticDependencyRead(ctx context.Context, tx *sql.Tx, project, channel string, frame frontFrame, found bool) automaticDependencyRead {
	if !found {
		return automaticDependencyRead{}
	}
	read := automaticDependencyRead{
		byNumber: make(map[int64]frontPull, len(frame.prefix)),
		facts:    make(map[int64][]any),
		load: func(p frontPull) ([]any, error) {
			return loadAutomaticDependencyFacts(ctx, tx, project, channel, frame.repository, p)
		},
	}
	for _, p := range frame.prefix {
		read.byNumber[p.Number] = p
	}
	return read
}

func (r automaticDependencyRead) forPull(pull int64) ([]any, error) {
	out := []any{}
	current, found := r.byNumber[pull]
	if !found {
		return out, nil
	}
	seen := map[int64]bool{pull: true}
	for parent := current.ParentPullNumber; parent != 0; {
		if seen[parent] || len(seen) > 100 {
			return nil, errors.Join(errAutomaticParentCycle, schedulingUnavailable())
		}
		seen[parent] = true
		p, exists := r.byNumber[parent]
		if !exists {
			out = append(out, []any{parent, "source_prefix_incomplete"})
			break
		}
		facts, loaded := r.facts[parent]
		if !loaded {
			var err error
			facts, err = r.load(p)
			if err != nil {
				return nil, err
			}
			r.facts[parent] = facts
		}
		out = append(out, facts)
		parent = p.ParentPullNumber
	}
	return out, nil
}

func loadAutomaticDependencyFacts(ctx context.Context, tx *sql.Tx, project, channel string, repository int64, p frontPull) ([]any, error) {
	parent := p.Number
	progress, err := channelProgress(ctx, tx, project, repository, parent, channel)
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
	return []any{parent, p.HeadSHA, p.ParentPullNumber, p.ParentSHA, progress, control.Configured, control.Epoch, control.grantRevision, control.Closed, control.Used, control.Dry, control.Tapered, completed, obligations, routes}, nil
}

// Planning can move on to a distinct unattempted PR without treating an
// operation's state transition as a reason to recreate that operation. A
// selected PR is suppressed by its own semantic basis, independently of peers.
func (e *Engine) automaticPlanningBasis(ctx context.Context, tx *sql.Tx, project string, c Configuration, ignoreSelection string) (string, bool, error) {
	return e.automaticPlanningBasisChannel(ctx, tx, project, c, ignoreSelection, "hosted")
}

func (e *Engine) automaticPlanningBasisChannel(ctx context.Context, tx *sql.Tx, project string, c Configuration, ignoreSelection string, channel string) (string, bool, error) {
	limit, err := e.automaticHostedQueuedPrefixChannel(ctx, tx, project, c, channel)
	if err != nil {
		return "", false, err
	}
	return automaticPlanningBasisForPrefix(ctx, tx, project, c.Queue[:limit], ignoreSelection, channel)
}

func automaticPlanningBasisForPrefix(ctx context.Context, tx *sql.Tx, project string, queue []int64, ignoreSelection, channel string) (string, bool, error) {
	var shared automaticBasisRead
	if len(queue) > 0 {
		var err error
		shared, err = readAutomaticBasis(ctx, tx, project, channel)
		if err != nil {
			return "", false, err
		}
	}
	values := make([]any, 0, len(queue))
	remaining := []int64{}
	degraded := false
	for _, pull := range queue {
		p, err := readPullScheduling(ctx, tx, project, pull)
		if err != nil {
			var local *pullSchedulingStateError
			if !errors.As(err, &local) || local.project != project || local.pull != pull {
				return "", false, err
			}
			// A missing readable control cannot establish this PR's basis.
			// Keep its position in the degraded digest and assess later PRs.
			values = append(values, []any{pull, nil, nil})
			degraded = true
			continue
		}
		basis, err := automaticPullBasisWithRead(ctx, tx, project, pull, channel, shared)
		if err != nil {
			var unreadable *automaticPullAdjudicationUnavailable
			if !p.Paused || !errors.As(err, &unreadable) {
				return "", false, err
			}
			// This is not a per-pull eligibility basis and it is never used
			// for a selected pull. Keep the paused row and its revision in
			// the degraded queue identity without pretending its hash exists.
			values = append(values, []any{pull, nil, p.Revision})
			degraded = true
			continue
		}
		values = append(values, []any{pull, basis})
		if p.Paused {
			continue
		}
		pending, err := automaticHostedPendingPullChannel(ctx, tx, project, pull, channel)
		if err != nil {
			return "", false, err
		}
		if pending {
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
	var digestInput any = []any{values, remaining}
	if degraded {
		// Preserve the paused-row encoding. An unreadable control has no
		// verified revision, so its null revision cannot match that case.
		digestInput = []any{"planning_basis_degraded_paused_v1", values, remaining}
	}
	sum := sha256.Sum256([]byte(encoded(digestInput)))
	basis := hex.EncodeToString(sum[:])
	if degraded && len(remaining) > 0 {
		// The coordinator treats an unseen basis as a fresh automatic
		// selection. An unresolved older lane hold without exact per-pull
		// attribution is not proof that the remaining work is distinct.
		var unresolved bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM review_automatic_selections a
			JOIN review_selections s ON s.id=a.selection_id AND s.project_id=a.project_id
			WHERE a.project_id=? AND s.channel=? AND a.selection_id<>?
			  AND s.state='held' AND s.operation_id IS NULL
			  AND (typeof(a.pull_number)<>'integer' OR a.pull_number NOT BETWEEN 1 AND 2147483647
			       OR typeof(a.pull_basis_sha256)<>'text' OR length(a.pull_basis_sha256)<>64
			       OR a.pull_basis_sha256 GLOB '*[^0-9a-f]*')
			  AND a.basis_sha256<>? AND a.settled_basis_sha256<>?
		)`, project, channel, ignoreSelection, basis, basis).Scan(&unresolved)
		if err != nil {
			return "", false, err
		}
		if unresolved {
			return basis, false, nil
		}
	}
	return basis, len(remaining) > 0, nil
}

// Legacy pre-child intents have only an opaque whole-queue digest. At a
// shortened cutoff there is no proof that a new prefix digest falls outside
// the retained intent. Keep that scope held until an explicit rearm changes
// the channel basis, a configuration/policy revision changes, or the full
// queue becomes eligible for the original exact-digest comparison.
func automaticLegacyPrefixFence(ctx context.Context, tx *sql.Tx, project string, configurationRevision, policyRevision int64, channel string) (bool, error) {
	basisRevision, err := automaticChannelBasisRevision(ctx, tx, project, channel)
	if err != nil {
		return false, err
	}
	var held bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM review_automatic_selections a
		JOIN review_selections s ON s.id=a.selection_id AND s.project_id=a.project_id
		WHERE a.project_id=? AND s.channel=? AND s.configuration_revision=? AND s.policy_revision=?
		  AND a.scheduling_revision>=? AND a.pull_number=0 AND s.operation_id IS NULL AND s.state IN ('held','empty')
		  AND NOT EXISTS(
		    SELECT 1 FROM events e WHERE e.project_id=s.project_id AND e.resource_id=s.id
		      AND e.event_type='review.selection_requested' AND e.revision=1 AND e.actor_id=s.actor_id
		      AND e.payload_json=json_object('channel',s.channel,'origin','automatic','worker_id',s.worker_id)
		  )
	)`, project, channel, configurationRevision, policyRevision, basisRevision).Scan(&held)
	return held, err
}

// A waiting request keeps its identity and deadline. Automatic selection may
// reconsider only earlier PRs, rather than duplicate that request or prefetch
// farther successors merely because the request is not executing yet.
func (e *Engine) automaticHostedQueuedPrefix(ctx context.Context, tx *sql.Tx, project string, c Configuration) (int, error) {
	return e.automaticHostedQueuedPrefixChannel(ctx, tx, project, c, "hosted")
}

func (e *Engine) automaticHostedQueuedPrefixChannel(ctx context.Context, tx *sql.Tx, project string, c Configuration, channel string) (int, error) {
	queue, err := json.Marshal(c.Queue)
	if err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT q.key,o.pull_number,s.id,s.project_id,o.id,EXISTS(SELECT 1 FROM review_selection_quarantine d WHERE d.project_id=o.project_id AND d.original_operation_id=o.id) FROM review_operations o
JOIN json_each(?) q ON q.value=o.pull_number
LEFT JOIN review_selections s ON s.operation_id=o.id
WHERE o.project_id=? AND o.channel=? AND o.state='queued'
AND NOT (o.channel='cli' AND o.reason='cli_draft_wait')
AND NOT EXISTS(SELECT 1 FROM review_operation_waits w WHERE w.operation_id=o.id AND w.paused=1)
AND NOT EXISTS(SELECT 1 FROM review_pull_scheduling s WHERE s.project_id=o.project_id AND s.pull_number=o.pull_number AND s.paused=1)
ORDER BY q.key,o.id LIMIT 101`, string(queue), project, channel)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for count := 0; rows.Next(); count++ {
		if count >= 100 {
			return 0, schedulingUnavailable()
		}
		var position int
		var pull int64
		var selection, selectionProject any
		var operation string
		var custodyPresent bool
		if err := rows.Scan(&position, &pull, &selection, &selectionProject, &operation, &custodyPresent); err != nil {
			return 0, err
		}
		if custodyPresent {
			if err := requireUnquarantinedSelectionLink(ctx, tx, project, operation); err != nil {
				if isLinkedSelectionCustodyHeld(err) {
					continue
				}
				return 0, err
			}
			return 0, runUnavailable()
		}
		// A readable direct operation keeps its fence. A locally
		// unavailable control cannot prevent independent pulls being assessed.
		if selection == nil {
			if _, err := readPullScheduling(ctx, tx, project, pull); err != nil {
				var local *pullSchedulingStateError
				if errors.As(err, &local) && local.project == project && local.pull == pull {
					continue
				}
				return 0, err
			}
			return position, nil
		}
		id, validID := selection.(string)
		if selectionProject != project || !validID || !validRunID(id) {
			return 0, schedulingUnavailable()
		}
		r, err := readRun(ctx, tx, project, id)
		if err != nil {
			var bindingState *operationSelectionBindingStateError
			if errors.As(err, &bindingState) {
				continue
			}
			return 0, err
		}
		if err = e.requireAutomaticRun(ctx, tx, r); err != nil {
			var failure *core.Error
			if errors.As(err, &failure) && failure.Code == "automation_authorization_changed" {
				// Only the cross-PR fence changes. The immutable bound
				// child is still pending for duplicate suppression.
				continue
			}
			return 0, err
		}
		return position, nil
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	return len(c.Queue), rows.Close()
}

func automaticHostedPendingPull(ctx context.Context, tx *sql.Tx, project string, pull int64) (bool, error) {
	return automaticHostedPendingPullChannel(ctx, tx, project, pull, "hosted")
}

func automaticHostedPendingPullChannel(ctx context.Context, tx *sql.Tx, project string, pull int64, channel string) (bool, error) {
	var pending bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_operations WHERE project_id=? AND pull_number=? AND channel='`+channel+`' AND state IN('queued','preparing','posting','active','uncertain'))`, project, pull).Scan(&pending)
	return pending, err
}

func (e *Engine) automaticPullSuppressed(ctx context.Context, tx *sql.Tx, r Run, pull int64) (bool, error) {
	// The fact-only evaluator may run without a persisted selection.
	if r.ID == "" {
		return false, nil
	}
	attribution, err := automaticSelectionBinding(ctx, tx, r)
	if err != nil || attribution.revision == 0 {
		return false, err
	}
	pending, err := automaticHostedPendingPullChannel(ctx, tx, r.ProjectID, pull, r.Channel)
	if err != nil || pending {
		return pending, err
	}
	c, _, err := readConfiguration(ctx, tx, r.ProjectID)
	if err != nil {
		return false, err
	}
	limit, err := e.automaticHostedQueuedPrefixChannel(ctx, tx, r.ProjectID, c, r.Channel)
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
	var seen bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_automatic_selections WHERE project_id=? AND selection_id<>? AND pull_number=? AND pull_basis_sha256=?)`, r.ProjectID, r.ID, pull, basis).Scan(&seen)
	return seen, err
}
func (e *Engine) settleAutomaticBasis(ctx context.Context, tx *sql.Tx, r Run, pull int64, acceptedOnly bool) error {
	attribution, err := automaticSelectionBinding(ctx, tx, r)
	if err != nil || attribution.revision == 0 {
		return err
	}
	c, _, err := readConfiguration(ctx, tx, r.ProjectID)
	if err != nil {
		return err
	}
	basis, _, err := e.automaticPlanningBasisChannel(ctx, tx, r.ProjectID, c, r.ID, r.Channel)
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
	if acceptedOnly {
		// A repository mismatch proves no new source facts. Retain the
		// original suppression only if the same basis is still current.
		_, err = tx.ExecContext(ctx, `UPDATE review_automatic_selections SET settled_basis_sha256=basis_sha256 WHERE selection_id=? AND project_id=? AND basis_sha256=?`, r.ID, r.ProjectID, basis)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE review_automatic_selections SET settled_basis_sha256=?,pull_number=?,pull_basis_sha256=? WHERE selection_id=? AND project_id=?`, basis, pull, pullBasis, r.ID, r.ProjectID)
	}
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
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// Leave the other selection worker available for accepted intents.
	if !e.automaticMu.TryLock() {
		return false, ctx.Err()
	}
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
	account    string
	scheduling schedulingRecord
	scope      core.Scope
	basis      string
	retry      *Run
}

func (e *Engine) planAutomaticProject(ctx context.Context, tx *sql.Tx, project, channel string, readers map[string]TargetReader) (*automaticProjectPlan, error) {
	var restored bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metadata WHERE (key='restore:provider_state' AND value='held') OR (key='restore:state' AND value='inactive'))`).Scan(&restored); err != nil {
		return nil, err
	}
	if restored {
		return nil, nil
	}
	s, err := readScheduling(ctx, tx, project)
	if err != nil || !automaticChannelEnabled(s, channel) {
		return nil, err
	}
	scope, err := e.store.ProjectAutomationScope(ctx, tx, project, s.actor, s.AuthorizingWorkerID, s.credential)
	if err != nil {
		var failure *core.Error
		if errors.As(err, &failure) && failure.Status >= 400 && failure.Status < 500 {
			return nil, nil
		}
		return nil, err
	}
	c, configurationRevision, err := readConfiguration(ctx, tx, project)
	if err != nil {
		var failure *core.Error
		if errors.As(err, &failure) && failure.Code == "reviews_unconfigured" {
			return nil, nil
		}
		return nil, err
	}
	if c.Paused || !slices.Contains(c.Channels, channel) || e.validateConfiguration(c) != nil || readers[c.CapabilityID] == nil || len(c.Queue) == 0 {
		return nil, nil
	}
	policy, err := readPolicy(ctx, tx, project)
	if err != nil {
		var failure *core.Error
		if errors.As(err, &failure) && failure.Code == "reviews_policy_unconfigured" {
			return nil, nil
		}
		return nil, err
	}
	if policy.Revision < 1 || slices.Contains(policy.Policy.PausedChannels, channel) {
		return nil, nil
	}
	var busy bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_selections WHERE project_id=? AND channel=? AND state IN('queued','selecting')) OR EXISTS(SELECT 1 FROM review_operations WHERE project_id=? AND channel=? AND state IN('preparing','posting','active','uncertain'))`, project, channel, project, channel).Scan(&busy); err != nil {
		return nil, err
	}
	if busy {
		return nil, nil
	}
	limit, err := e.automaticHostedQueuedPrefixChannel(ctx, tx, project, c, channel)
	if err != nil || limit == 0 {
		return nil, err
	}
	queue := c.Queue[:limit]
	unpaused := false
	for _, pull := range queue {
		control, err := readPullScheduling(ctx, tx, project, pull)
		if err != nil {
			return nil, err
		}
		unpaused = unpaused || !control.Paused
	}
	if !unpaused {
		return nil, nil
	}
	basis, candidates, err := automaticPlanningBasisForPrefix(ctx, tx, project, queue, "", channel)
	if err != nil || !candidates {
		return nil, err
	}
	var seen bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_automatic_selections WHERE project_id=? AND (basis_sha256=? OR settled_basis_sha256=?))`, project, basis, basis).Scan(&seen); err != nil {
		return nil, err
	}
	plan := &automaticProjectPlan{account: automaticAccountKey(e.capabilities[c.CapabilityID]), scheduling: s, scope: scope, basis: basis}
	if seen {
		plan.retry, err = e.automaticSourceRetryCandidate(ctx, tx, project, basis, s.Revision, time.Now(), channel)
		if err != nil || plan.retry == nil {
			return nil, err
		}
	}
	if !seen && limit < len(c.Queue) {
		held, err := automaticLegacyPrefixFence(ctx, tx, project, configurationRevision, policy.Revision, channel)
		if err != nil || held {
			return nil, err
		}
	}
	return plan, nil
}

func automaticAccountKey(capability Capability) string {
	return capability.Provider + "\x00" + capability.Account
}

func (e *Engine) generateAutomaticProject(ctx context.Context, project, channel string, readers map[string]TargetReader) (bool, error) {
	return e.generateAutomaticProjectForAccount(ctx, project, channel, readers, "")
}

// A CLI pass nominates only under the account counted by its pre-read;
// a changed account must be reconsidered by a fresh pass.
func (e *Engine) generateAutomaticProjectForAccount(ctx context.Context, project, channel string, readers map[string]TargetReader, expectedAccount string) (bool, error) {
	var candidate *automaticProjectPlan
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		candidate, err = e.planAutomaticProject(ctx, tx, project, channel, readers)
		return err
	})
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil || candidate == nil || expectedAccount != "" && candidate.account != expectedAccount {
		return false, err
	}
	return e.acceptAutomaticProject(ctx, project, channel, readers, candidate)
}

func (e *Engine) acceptAutomaticProject(ctx context.Context, project, channel string, readers map[string]TargetReader, candidate *automaticProjectPlan) (bool, error) {
	return e.acceptAutomaticProjectWithEnqueue(ctx, project, channel, readers, candidate, e.enqueueRunTx)
}

func (e *Engine) acceptAutomaticProjectWithEnqueue(ctx context.Context, project, channel string, readers map[string]TargetReader, candidate *automaticProjectPlan, enqueue func(context.Context, *sql.Tx, core.Scope, RunRequest) (Receipt, error)) (bool, error) {
	worked := false
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := e.planAutomaticProject(ctx, tx, project, channel, readers)
		if err != nil || current == nil {
			return err
		}
		if channel == "cli" && current.account != candidate.account || current.basis != candidate.basis || current.scheduling.Revision != candidate.scheduling.Revision || (current.retry == nil) != (candidate.retry == nil) {
			return nil
		}
		if current.retry != nil {
			if current.retry.ID != candidate.retry.ID || current.retry.Revision != candidate.retry.Revision {
				return nil
			}
			err = updateRunAt(ctx, tx, *current.retry, "queued", "", "", time.Now())
		} else {
			// Same ordinary acceptance path, fully reauthorized in this snapshot.
			receipt, enqueueErr := enqueue(ctx, tx, current.scope, RunRequest{Channel: channel, origin: selectionOriginAutomatic})
			if enqueueErr != nil {
				var failure *core.Error
				if errors.As(enqueueErr, &failure) && failure.Status == 409 {
					return nil
				}
				return enqueueErr
			}
			s := current.scheduling
			_, err = tx.ExecContext(ctx, `INSERT INTO review_automatic_selections(selection_id,project_id,scheduling_revision,basis_sha256,actor_id,worker_id,credential_id) VALUES(?,?,?,?,?,?,?)`, receipt.ID, project, s.Revision, current.basis, s.actor, s.AuthorizingWorkerID, s.credential)
		}
		worked = err == nil
		return err
	})
	if ctx.Err() != nil {
		return worked && err == nil, ctx.Err()
	}
	return worked && err == nil, err
}
