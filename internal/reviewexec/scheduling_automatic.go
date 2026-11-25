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

func automaticChannelEnabled(s schedulingRecord, channel string) bool {
	return channel == "hosted" && s.AutomaticHosted || channel == "cli" && s.AutomaticCLI
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
		// Same-revision work still requires the exact canonical event
		// behind its enabled control; a retained projection alone is not proof.
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
	prefix    []byte
	revision  int64
	workspace bool
	frame     frontFrame
	hasFrame  bool
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
	out.frame, out.hasFrame, err = readFrontFrame(ctx, tx, project, channel)
	if err != nil {
		return out, err
	}
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
	dependencies, err := automaticDependenciesWithFrame(ctx, tx, project, pull, channel, shared.frame, shared.hasFrame)
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
	return automaticDependenciesWithFrame(ctx, tx, project, pull, channel, frame, found)
}

func automaticDependenciesWithFrame(ctx context.Context, tx *sql.Tx, project string, pull int64, channel string, frame frontFrame, found bool) ([]any, error) {
	out := []any{}
	if !found {
		return out, nil
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
			return "", false, err
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
		// Version the encoding so it cannot be treated as an equivalent
		// full-queue basis or rearm a full-digest-only historical intent.
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
	rows, err := tx.QueryContext(ctx, `SELECT q.key,s.id,s.project_id,o.id,EXISTS(SELECT 1 FROM review_selection_quarantine d WHERE d.project_id=o.project_id AND d.original_operation_id=o.id) FROM review_operations o
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
		var selection, selectionProject any
		var operation string
		var custodyPresent bool
		if err := rows.Scan(&position, &selection, &selectionProject, &operation, &custodyPresent); err != nil {
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
		// A direct operation has no parent selection and keeps its position.
		if selection == nil {
			return position, nil
		}
		id, validID := selection.(string)
		if selectionProject != project || !validID || !validRunID(id) {
			return 0, schedulingUnavailable()
		}
		r, err := readRun(ctx, tx, project, id)
		if err != nil {
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
func (e *Engine) settleAutomaticBasis(ctx context.Context, tx *sql.Tx, r Run, pull int64) error {
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
	_, err = tx.ExecContext(ctx, `UPDATE review_automatic_selections SET settled_basis_sha256=?,pull_number=?,pull_basis_sha256=? WHERE selection_id=? AND project_id=?`, basis, pull, pullBasis, r.ID, r.ProjectID)
	return err
}
