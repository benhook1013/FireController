package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

// A qualified original release permits at most one fresh automatic child.
// Children never mint a new root. Original failed operations are immutable.
const hostedContinuationDelay = 2 * time.Minute

type hostedContinuation struct {
	Root, Project, Operation, Capture, AnchorJSON, Due, Selection, Child string
	Revision, Scheduling, Configuration, Policy, Event                   int64
	Anchor                                                               Anchor
}

const continuationColumns = `root_attempt,project_id,root_operation,release_revision,capture_sha256,anchor_json,scheduling_revision,configuration_revision,policy_revision,accepted_sequence,not_before,coalesce(child_selection,''),coalesce(child_operation,'')`

func hostedContinuationSchema(ctx context.Context, s *core.Store) error {
	return s.EnsureSchema(ctx, "review_hosted_continuations", []string{`CREATE TABLE review_hosted_continuations(root_attempt TEXT PRIMARY KEY REFERENCES review_execution_attempts(id),project_id TEXT NOT NULL REFERENCES projects(id),root_operation TEXT NOT NULL UNIQUE REFERENCES review_operations(id),release_revision INTEGER NOT NULL CHECK(release_revision>2),capture_sha256 TEXT NOT NULL,anchor_json TEXT NOT NULL,scheduling_revision INTEGER NOT NULL CHECK(scheduling_revision>0),configuration_revision INTEGER NOT NULL CHECK(configuration_revision>0),policy_revision INTEGER NOT NULL CHECK(policy_revision>0),accepted_sequence INTEGER NOT NULL CHECK(accepted_sequence>0),not_before TEXT NOT NULL,child_selection TEXT UNIQUE REFERENCES review_selections(id),child_operation TEXT UNIQUE REFERENCES review_operations(id),CHECK(child_operation IS NULL OR child_selection IS NOT NULL))`})
}
func scanContinuation(row interface{ Scan(...any) error }) (*hostedContinuation, error) {
	c := new(hostedContinuation)
	err := row.Scan(&c.Root, &c.Project, &c.Operation, &c.Revision, &c.Capture, &c.AnchorJSON, &c.Scheduling, &c.Configuration, &c.Policy, &c.Event, &c.Due, &c.Selection, &c.Child)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !hash.MatchString(c.Capture) || json.Unmarshal([]byte(c.AnchorJSON), &c.Anchor) != nil || !validEvidenceAnchor(c.Anchor) || encoded(c.Anchor) != c.AnchorJSON {
		return nil, waitUnavailable()
	}
	if _, ok := attemptRecordedTime(c.Due); !ok {
		return nil, waitUnavailable()
	}
	return c, nil
}
func continuationForSelection(ctx context.Context, q EvidenceQuery, project, selection string) (*hostedContinuation, error) {
	return scanContinuation(q.QueryRowContext(ctx, `SELECT `+continuationColumns+` FROM review_hosted_continuations WHERE project_id=? AND child_selection=?`, project, selection))
}

// Invoked only inside the existing audited release transaction, after fresh
// original-capture decoding proved finished with no recoverable inventory.
func (e *Engine) qualifyHostedContinuation(ctx context.Context, tx *sql.Tx, a Attempt, o Operation, source HostedIncompleteCapture, now time.Time) error {
	var child bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_hosted_continuations WHERE child_operation=?)`, o.ID).Scan(&child); err != nil {
		return err
	}
	if child {
		return nil
	}
	s, err := readScheduling(ctx, tx, o.ProjectID)
	if err != nil {
		return err
	}
	if !s.AutomaticHosted {
		return nil
	}
	if _, err = e.store.ProjectAutomationScope(ctx, tx, o.ProjectID, s.actor, s.AuthorizingWorkerID, s.credential); err != nil {
		var f *core.Error
		if errors.As(err, &f) && f.Status < 500 {
			return nil
		}
		return err
	}
	if _, err = e.currentExecutionContext(ctx, tx, o); err != nil {
		var f *core.Error
		if errors.As(err, &f) && f.Status < 500 {
			return nil
		}
		return err
	}
	var event int64
	if err = tx.QueryRowContext(ctx, `SELECT sequence FROM events WHERE project_id=? AND event_type='review.requested' AND resource_id=?`, o.ProjectID, o.ID).Scan(&event); err != nil {
		return err
	}
	due := now.Add(hostedContinuationDelay).UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT INTO review_hosted_continuations(root_attempt,project_id,root_operation,release_revision,capture_sha256,anchor_json,scheduling_revision,configuration_revision,policy_revision,accepted_sequence,not_before) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, a.ID, o.ProjectID, o.ID, a.Revision+1, source.SHA256, encoded(*a.Anchor), s.Revision, o.ConfigurationRevision, o.PolicyRevision, event, due)
	if err != nil {
		return err
	}
	c := &hostedContinuation{Operation: o.ID, Capture: source.SHA256, AnchorJSON: encoded(*a.Anchor), Scheduling: s.Revision, Configuration: o.ConfigurationRevision, Policy: o.PolicyRevision, Event: event, Due: due}
	return core.AppendEvent(ctx, tx, core.Scope{ProjectID: o.ProjectID, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, "review.hosted_continuation_qualified", a.ID, a.Revision+1, continuationProvenance(c))
}

// A later explicit request supersedes even when it has already been cancelled
// or failed. Sequence ordering, not wall time, fences same-timestamp races.
func (e *Engine) validateHostedContinuation(ctx context.Context, tx *sql.Tx, c *hostedContinuation) error {
	if err := validateContinuationAudit(ctx, tx, c); err != nil {
		return err
	}
	a, err := readAttempt(ctx, tx, c.Project, c.Root)
	if err != nil {
		return err
	}
	o, err := readOperationBase(ctx, tx, c.Project, c.Operation)
	if err != nil {
		return err
	}
	if o.ConfigurationRevision != c.Configuration || o.PolicyRevision != c.Policy {
		return waitUnavailable()
	}
	if a.State != "failed" || a.Revision != c.Revision || a.OperationID != o.ID || o.State != "failed" || o.AttemptID != a.ID || o.Reason != HostedFinishedIncompleteReason || a.Anchor == nil || encoded(*a.Anchor) != c.AnchorJSON {
		return sourceHold("hosted_continuation_changed")
	}
	if err = validateConfirmedHostedSettlement(ctx, tx, a, o); err != nil {
		return err
	}
	if err = validateSettlementObservations(ctx, tx, a, o, false); err != nil {
		return err
	}
	var capture string
	if err = tx.QueryRowContext(ctx, `SELECT capture_sha256 FROM review_hosted_observations WHERE attempt_id=? ORDER BY position DESC LIMIT 1`, a.ID).Scan(&capture); err != nil {
		return err
	}
	if capture != c.Capture {
		return sourceHold("hosted_continuation_changed")
	}
	var occupied bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_operations WHERE project_id=? AND channel='hosted' AND pull_number=? AND id<>? AND id<>? AND state IN('queued','preparing','posting','active','uncertain')) OR EXISTS(SELECT 1 FROM review_execution_evidence WHERE attempt_id=?) OR EXISTS(SELECT 1 FROM events e JOIN review_operations o ON o.id=e.resource_id AND o.project_id=e.project_id WHERE e.project_id=? AND e.sequence>? AND e.event_type='review.requested' AND o.channel='hosted' AND o.pull_number=? AND o.id<>? AND o.id<>?) OR EXISTS(SELECT 1 FROM events e JOIN review_selections s ON s.id=e.resource_id AND s.project_id=e.project_id WHERE e.project_id=? AND e.sequence>? AND e.event_type='review.selection_requested' AND s.channel='hosted' AND s.id<>? AND NOT EXISTS(SELECT 1 FROM review_automatic_selections auto WHERE auto.project_id=s.project_id AND auto.selection_id=s.id) AND (s.expected_pull_number=0 OR s.expected_pull_number=?))`, c.Project, a.Anchor.PullNumber, c.Operation, c.Child, a.ID, c.Project, c.Event, a.Anchor.PullNumber, c.Operation, c.Child, c.Project, c.Event, c.Selection, a.Anchor.PullNumber).Scan(&occupied); err != nil {
		return err
	}
	if occupied {
		return sourceHold("hosted_continuation_superseded")
	}
	s, err := readScheduling(ctx, tx, c.Project)
	if err != nil {
		return err
	}
	if !s.AutomaticHosted || s.Revision != c.Scheduling {
		return automaticDenied()
	}
	if _, err = e.store.ProjectAutomationScope(ctx, tx, c.Project, s.actor, s.AuthorizingWorkerID, s.credential); err != nil {
		return err
	}
	config, revision, err := readConfiguration(ctx, tx, c.Project)
	if err != nil {
		return err
	}
	if config.Paused || revision != c.Configuration {
		return sourceHold("configuration_changed")
	}
	policy, err := readPolicy(ctx, tx, c.Project)
	if err != nil {
		return err
	}
	if policy.Revision != c.Policy {
		return sourceHold("policy_changed")
	}
	if err = requirePullScheduling(ctx, tx, c.Project, a.Anchor.PullNumber); err != nil {
		return err
	}
	allowed, err := actorAllowed(ctx, tx, c.Project, o.ActorID, o.WorkerID)
	if err != nil {
		return err
	}
	if !allowed {
		return automaticDenied()
	}
	return nil
}
func continuationMatches(c *hostedContinuation, p *SourcePlan) bool {
	a := c.Anchor
	return p.Repository.ID == a.RepositoryID && p.Pull.Number == a.PullNumber && p.Pull.HeadSHA == a.CandidateSHA && p.Pull.BaseSHA == a.BaseSHA && p.Pull.BaseRef == a.BaseRef && p.ParentSHA == a.ParentSHA && p.ParentPullNumber == a.ParentPullNumber
}
func (e *Engine) pendingHostedContinuation(ctx context.Context, tx *sql.Tx, project string, config Configuration) (*hostedContinuation, error) {
	for _, pull := range config.Queue {
		rows, err := tx.QueryContext(ctx, `SELECT `+continuationColumns+` FROM review_hosted_continuations WHERE project_id=? AND child_operation IS NULL AND json_extract(anchor_json,'$.pull_number')=? ORDER BY accepted_sequence DESC LIMIT 2`, project, pull)
		if err != nil {
			return nil, err
		}
		var candidates []*hostedContinuation
		for rows.Next() {
			c, err := scanContinuation(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			candidates = append(candidates, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		for _, c := range candidates {
			if e.sampleExecutionTime().Before(mustWaitTime(c.Due)) {
				continue
			}
			if c.Selection != "" {
				r, err := readRun(ctx, tx, project, c.Selection)
				if err != nil {
					return nil, err
				}
				if !continuationSelectionWait(r) || e.sampleExecutionTime().Before(mustWaitTime(r.UpdatedAt).Add(hostedContinuationDelay)) {
					continue
				}
			}
			if err = e.validateHostedContinuation(ctx, tx, c); err != nil {
				var f *core.Error
				if errors.As(err, &f) && f.Status < 500 {
					continue
				}
				return nil, err
			}
			return c, nil
		}
	}
	return nil, nil
}
func continuationBasis(c *hostedContinuation) string {
	s := sha256.Sum256([]byte("hosted-continuation-v1:" + c.Root))
	return hex.EncodeToString(s[:])
}

// ValidateHostedContinuationsSnapshot validates provenance and links without
// requiring live authorization (revocation is a legitimate retained state).
func ValidateHostedContinuationsSnapshot(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+continuationColumns+` FROM review_hosted_continuations`)
	if err != nil {
		return err
	}
	var all []*hostedContinuation
	for rows.Next() {
		c, err := scanContinuation(rows)
		if err != nil {
			rows.Close()
			return err
		}
		all = append(all, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range all {
		a, err := readAttempt(ctx, tx, c.Project, c.Root)
		if err != nil {
			return err
		}
		o, err := readOperationBase(ctx, tx, c.Project, c.Operation)
		if err != nil {
			return err
		}
		if a.OperationID != o.ID || a.Anchor == nil || encoded(*a.Anchor) != c.AnchorJSON || o.Channel != "hosted" || a.Revision < c.Revision || o.ConfigurationRevision != c.Configuration || o.PolicyRevision != c.Policy {
			return waitUnavailable()
		}
		if err := validateContinuationAudit(ctx, tx, c); err != nil {
			return err
		}
		var state, reason, external string
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT state,reason,external_id FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, c.Root, c.Revision).Scan(&state, &reason, &external); err != nil {
			return err
		}
		if state != "failed" || reason != HostedFinishedIncompleteReason || external != a.ExternalID {
			return waitUnavailable()
		}
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM review_hosted_continuations WHERE child_operation=?`, c.Operation).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return waitUnavailable()
		}
		if c.Selection != "" {
			r, err := readRun(ctx, tx, c.Project, c.Selection)
			if err != nil {
				return err
			}
			if r.Channel != "hosted" || r.ExpectedPullNumber != c.Anchor.PullNumber || r.ConfigurationRevision != c.Configuration || r.PolicyRevision != c.Policy || r.OperationID != c.Child {
				return waitUnavailable()
			}
			var basis string
			var scheduling int64
			if err = tx.QueryRowContext(ctx, `SELECT basis_sha256,scheduling_revision FROM review_automatic_selections WHERE project_id=? AND selection_id=?`, c.Project, c.Selection).Scan(&basis, &scheduling); err != nil {
				return err
			}
			if basis != continuationBasis(c) || scheduling != c.Scheduling {
				return waitUnavailable()
			}
		} else if c.Child != "" {
			return waitUnavailable()
		}
		if c.Child != "" {
			child, err := readOperationBase(ctx, tx, c.Project, c.Child)
			if err != nil {
				return err
			}
			if child.Channel != "hosted" || child.PullNumber != c.Anchor.PullNumber || child.HeadSHA != c.Anchor.CandidateSHA {
				return waitUnavailable()
			}
		}
	}
	return nil
}

func (e *Engine) requireHostedContinuationAnchor(ctx context.Context, tx *sql.Tx, o Operation, anchor Anchor) error {
	c, err := scanContinuation(tx.QueryRowContext(ctx, `SELECT `+continuationColumns+` FROM review_hosted_continuations WHERE project_id=? AND child_operation=?`, o.ProjectID, o.ID))
	if err != nil || c == nil {
		return err
	}
	if !hostedAllowanceSameSource(c.Anchor, anchor) {
		return sourceHold("source_changed")
	}
	return e.validateHostedContinuation(ctx, tx, c)
}

func validateContinuationAudit(ctx context.Context, q EvidenceQuery, c *hostedContinuation) error {
	var body string
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*),coalesce(max(payload_json),'') FROM events WHERE project_id=? AND resource_id=? AND revision=? AND event_type='review.hosted_continuation_qualified'`, c.Project, c.Root, c.Revision).Scan(&n, &body); err != nil {
		return err
	}
	if n != 1 || body != encoded(continuationProvenance(c)) {
		return waitUnavailable()
	}

	if err := q.QueryRowContext(ctx, `SELECT count(*),coalesce(max(payload_json),'') FROM events WHERE project_id=? AND resource_id=? AND revision=? AND event_type='review.hosted_incomplete_released'`, c.Project, c.Root, c.Revision).Scan(&n, &body); err != nil {
		return err
	}
	var release struct {
		OperationID string `json:"operation_id"`
		CaptureSHA  string `json:"capture_sha256"`
		ReleasedAt  string `json:"released_at"`
	}
	if n != 1 || json.Unmarshal([]byte(body), &release) != nil || release.OperationID != c.Operation || release.CaptureSHA != c.Capture {
		return waitUnavailable()
	}
	released, valid := attemptRecordedTime(release.ReleasedAt)
	if !valid || released.Add(hostedContinuationDelay).Format(time.RFC3339Nano) != c.Due {
		return waitUnavailable()
	}
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE project_id=? AND sequence=? AND resource_id=? AND event_type='review.requested'`, c.Project, c.Event, c.Operation).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return waitUnavailable()
	}
	return nil
}

func continuationRefusalReason(err error) string {
	var f *core.Error
	if !errors.As(err, &f) {
		return ""
	}
	if f.Status == 403 {
		return "automation_authorization_changed"
	}
	if f.Code != "review_held" {
		return ""
	}
	d, ok := f.Details.(map[string]string)
	if !ok {
		return ""
	}
	switch d["reason"] {
	case "hosted_continuation_changed", "hosted_continuation_superseded", "configuration_changed", "policy_changed", "source_changed":
		return d["reason"]
	}
	return ""
}

// A read-only selection blocked by another queue front keeps the same receipt
// and selection. This never refunds or replenishes a provider attempt budget.
func continuationSelectionWait(r Run) bool {
	if r.State != "held" || r.OperationID != "" {
		return false
	}
	switch r.Reason {
	case "expected_pr_mismatch", "selection_changed", "source_prefix_incomplete":
		return true
	}
	return false
}

// Ordinary automatic selections must not race a retained continuation for its
// own target while serving earlier work. Explicit manual requests remain free
// to supersede it through their existing guarded path.
func (e *Engine) continuationOwnsPull(ctx context.Context, tx *sql.Tx, project string, pull int64) (bool, error) {
	c, err := scanContinuation(tx.QueryRowContext(ctx, `SELECT `+continuationColumns+` FROM review_hosted_continuations WHERE project_id=? AND child_operation IS NULL AND json_extract(anchor_json,'$.pull_number')=? ORDER BY accepted_sequence DESC LIMIT 1`, project, pull))
	if err != nil || c == nil {
		return false, err
	}
	if c.Selection != "" {
		r, err := readRun(ctx, tx, project, c.Selection)
		if err != nil {
			return false, err
		}
		if r.State != "queued" && r.State != "selecting" && !continuationSelectionWait(r) {
			return false, nil
		}
	}
	if err = e.validateHostedContinuation(ctx, tx, c); err != nil {
		var f *core.Error
		if errors.As(err, &f) && f.Status < 500 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func continuationProvenance(c *hostedContinuation) any {
	anchor := sha256.Sum256([]byte(c.AnchorJSON))
	return struct {
		Operation     string `json:"operation_id"`
		Capture       string `json:"capture_sha256"`
		Anchor        string `json:"anchor_sha256"`
		Scheduling    int64  `json:"scheduling_revision"`
		Configuration int64  `json:"configuration_revision"`
		Policy        int64  `json:"policy_revision"`
		Event         int64  `json:"accepted_sequence"`
		Due           string `json:"not_before"`
	}{c.Operation, c.Capture, hex.EncodeToString(anchor[:]), c.Scheduling, c.Configuration, c.Policy, c.Event, c.Due}
}
