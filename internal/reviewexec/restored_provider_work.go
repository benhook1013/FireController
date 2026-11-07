package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

// RestoredWorkSummary counts local abandonments, never external cancellations
// or completed review credit.
type RestoredWorkSummary struct {
	Selections       int64 `json:"selections,omitempty"`
	QueuedOperations int64 `json:"queued_operations"`
	Attempts         int64 `json:"attempts"`
	SourceRefreshes  int64 `json:"source_refreshes"`
	Publications     int64 `json:"publications"`
	PausedProjects   int64 `json:"paused_projects"`
}

const restoredWorkLimit = 10000

func restoredWorkInvalid() error {
	return core.Fail("restore_reconciliation_invalid", "Retained provider work is inconsistent; no provider release was authorized.", 409)
}
func restoredWorkSchema() error {
	return core.Fail("restore_reconciliation_schema", "Start the current daemon with providers held to migrate retained work, then stop it and retry reconciliation.", 409)
}

// AbandonRestoredProviderWork participates in the caller's offline transaction.
// The caller owns validated recovery authority and MUST roll back on any error.
// It does not initialize domains, authorize external effects or contact providers.
func AbandonRestoredProviderWork(ctx context.Context, tx *sql.Tx, instanceID string, at time.Time) (result RestoredWorkSummary, err error) {
	if ctx == nil || tx == nil || !identifier.MatchString(instanceID) || at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
		return result, restoredWorkInvalid()
	}
	// SQL and retained-data guards below deliberately expose only safe domain
	// failures. Normalize interruption at this outer boundary so early schema or
	// metadata reads cannot misclassify cancellation as inconsistent data. The
	// caller must still roll back this entire transaction on the returned error.
	defer func() {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			result = RestoredWorkSummary{}
			err = core.Fail("restore_reconciliation_interrupted", "Restore reconciliation was interrupted; no provider release was recorded. Retry the exact reconciliation command.", 503)
		}
	}()
	for key, want := range map[string]string{"instance_id": instanceID, "restore:state": "coordination_active", "restore:provider_state": "held"} {
		var got string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, key).Scan(&got); err != nil || got != want {
			return result, restoredWorkInvalid()
		}
	}
	execution, err := restoredDomain(ctx, tx, "review_execution", "review_operations", "6")
	if err != nil {
		return result, err
	}
	source, err := restoredDomain(ctx, tx, "review_source_refresh", "review_source_refreshes", "3")
	if err != nil {
		return result, err
	}
	checkpoint, err := restoredDomain(ctx, tx, "review_checkpoint_publication", "review_checkpoint_publications", "1")
	if err != nil {
		return result, err
	}
	if (source || checkpoint) && !execution {
		return result, restoredWorkSchema()
	}
	// Present domains must already have the current read/write shape. An offline
	// release never attempts a schema migration or creates a missing companion.
	checks := []struct {
		enabled bool
		query   string
	}{
		{execution, `SELECT o.capability_fingerprint,o.policy_revision,a.anchor_json,r.action_class,t.external_id,c.body_json,q.body_json FROM review_operations o,review_execution_attempts a,review_execution_reservations r,review_execution_transitions t,review_execution_config c,review_hosted_allowance_queries q LIMIT 0`},
		{source, `SELECT ` + refreshMetadataColumns + `,ci_observation_json,review_state_json FROM review_source_refreshes LIMIT 0`},
		{checkpoint, `SELECT p.body_json,r.body_json,i.body_json FROM review_checkpoint_publications p,review_checkpoint_revisions r,review_checkpoint_intents i LIMIT 0`},
	}
	for _, check := range checks {
		if check.enabled {
			rows, err := tx.QueryContext(ctx, check.query)
			if err != nil {
				return result, restoredWorkSchema()
			}
			if err = rows.Close(); err != nil {
				return result, restoredWorkSchema()
			}
		}
	}

	guards := []struct {
		enabled bool
		query   string
	}{
		{execution, `SELECT EXISTS(SELECT 1 FROM review_operations WHERE state NOT IN ('queued','preparing','posting','active','uncertain','held','failed','completed','cancelled','rate_limited')) OR EXISTS(SELECT 1 FROM review_execution_attempts WHERE state NOT IN ('preparing','posting','active','uncertain','held','failed','completed','cancelled','rate_limited'))`},
		{source, `SELECT EXISTS(SELECT 1 FROM review_source_refreshes WHERE state NOT IN ('queued','running','completed','held'))`},
	}
	for _, guard := range guards {
		if guard.enabled {
			var invalid bool
			if err := tx.QueryRowContext(ctx, guard.query).Scan(&invalid); err != nil || invalid {
				return result, restoredWorkInvalid()
			}
		}
	}

	if execution {
		if err = abandonRestoredAttempts(ctx, tx, at, &result); err == nil {
			err = pauseRestoredConfigurations(ctx, tx, &result)
		}
	}
	if err == nil && source {
		err = abandonRestoredSources(ctx, tx, at, &result)
	}
	if err == nil && checkpoint {
		err = abandonRestoredPublications(ctx, tx, at, &result)
	}
	if err != nil {
		var failure *core.Error
		if errors.As(err, &failure) {
			return RestoredWorkSummary{}, err
		}
		return RestoredWorkSummary{}, restoredWorkInvalid()
	}
	return result, nil
}

func restoredDomain(ctx context.Context, tx *sql.Tx, domain, table, version string) (bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name=?)`, table).Scan(&exists); err != nil {
		return false, restoredWorkInvalid()
	}
	var v string
	err := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, "schema:"+domain).Scan(&v)
	if !exists && errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || !exists || v != version {
		return false, restoredWorkSchema()
	}
	return true, nil
}

type restoredIdentity struct{ project, id string }

func restoredSelected(ctx context.Context, tx *sql.Tx, query string) ([]restoredIdentity, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []restoredIdentity
	for rows.Next() {
		var v restoredIdentity
		if err = rows.Scan(&v.project, &v.id); err != nil {
			return nil, err
		}
		out = append(out, v)
		if len(out) > restoredWorkLimit {
			return nil, core.Fail("restore_reconciliation_capacity", "Retained provider work exceeds the local reconciliation bound.", 409)
		}
	}
	return out, rows.Err()
}

func restoredOperationValid(o Operation) bool {
	return identifier.MatchString(o.ID) && identifier.MatchString(o.ProjectID) && identifier.MatchString(o.ActorID) && (o.WorkerID == "" || identifier.MatchString(o.WorkerID)) && o.ConfigurationRevision > 0 && o.PolicyRevision >= 0 && hash.MatchString(o.CapabilityFingerprint) && o.PullNumber > 0 && o.PullNumber <= 2147483647 && validChannel(o.Channel) && sha.MatchString(o.HeadSHA) && o.Revision > 0 && o.Revision < math.MaxInt64 && (o.Reason == "" || identifier.MatchString(o.Reason))
}
func abandonRestoredAttempts(ctx context.Context, tx *sql.Tx, at time.Time, summary *RestoredWorkSummary) error {
	var projection bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='review_workspace')`).Scan(&projection); err != nil {
		return err
	}
	journal := &Engine{}
	if projection {
		journal.publishAttempt = publishAttemptTransition
	}
	// Select through both sides so an orphaned/mislinked pending attempt cannot
	// disappear from reconciliation merely because its operation looks terminal.
	queued, err := restoredSelected(ctx, tx, `SELECT project_id,id FROM review_operations WHERE state='queued' ORDER BY id LIMIT 10001`)
	if err != nil {
		return err
	}
	pending, err := restoredSelected(ctx, tx, `SELECT project_id,id FROM review_execution_attempts WHERE state IN ('preparing','posting','active','uncertain') ORDER BY id LIMIT 10001`)
	if err != nil {
		return err
	}
	var invalid bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_operations o LEFT JOIN review_execution_attempts a ON a.id=o.attempt_id WHERE o.state IN ('preparing','posting','active','uncertain') AND (a.id IS NULL OR a.operation_id<>o.id OR a.project_id<>o.project_id OR a.state<>o.state)) OR EXISTS(SELECT 1 FROM review_execution_reservations r LEFT JOIN review_execution_attempts a ON a.id=r.attempt_id WHERE a.id IS NULL OR a.state NOT IN ('preparing','posting','active','uncertain'))`).Scan(&invalid); err != nil {
		return err
	}
	if invalid {
		return restoredWorkInvalid()
	}
	for _, id := range queued {
		o, err := readOperation(ctx, tx, id.project, id.id)
		if err != nil {
			return err
		}
		var linked int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM review_execution_attempts WHERE operation_id=?`, o.ID).Scan(&linked); err != nil {
			return err
		}
		if !restoredOperationValid(o) || o.AttemptID != "" || linked != 0 {
			return restoredWorkInvalid()
		}
		stamp, err := attemptRecordingStamp(ctx, tx, o, nil, at)
		if err != nil {
			return err
		}
		if _, err = sourceConfiguration(ctx, tx, o.ProjectID, o.ConfigurationRevision); err != nil {
			return err
		}
		if err = holdOperation(ctx, tx, o, "restore_reconciled", stamp); err != nil {
			return err
		}
		summary.QueuedOperations++
	}
	for _, id := range pending {
		a, err := readAttempt(ctx, tx, id.project, id.id)
		if err != nil {
			return err
		}
		o, err := readOperation(ctx, tx, id.project, a.OperationID)
		if err != nil {
			return err
		}
		if !restoredOperationValid(o) || a.ID != o.AttemptID || a.ProjectID != o.ProjectID || a.OperationID != o.ID || a.State != o.State || a.Revision < 1 || a.Revision >= math.MaxInt64-1 || o.Revision != a.Revision+1 || !hostedObservationID(a.ID, "review_attempt_") || !identifier.MatchString(a.Provider) || !identifier.MatchString(a.Account) || (a.ExternalID != "" && !identifier.MatchString(a.ExternalID)) {
			return restoredWorkInvalid()
		}
		stamp, err := attemptRecordingStamp(ctx, tx, o, &a, at)
		if err != nil {
			return err
		}
		if err = validateRestoredAttempt(ctx, tx, a, o); err != nil {
			return err
		}
		if a.State == "posting" && o.Revision >= math.MaxInt64-1 {
			return restoredWorkInvalid()
		}
		states := []string{"cancelled"}
		if a.State == "posting" {
			states = []string{"uncertain", "cancelled"}
		}
		for _, state := range states {
			if !allowedTransition(a.State, state) {
				return restoredWorkInvalid()
			}
			a.State = state
			a.Revision++
			if _, err = tx.ExecContext(ctx, `UPDATE review_execution_attempts SET state=?,revision=? WHERE id=?`, state, a.Revision, a.ID); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE review_operations SET state=?,reason='restore_reconciled',revision=revision+1,updated_at=? WHERE id=?`, state, stamp, o.ID); err != nil {
				return err
			}
			if err = journal.transitionRow(ctx, tx, a, "restore_reconciled", stamp); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM review_execution_reservations WHERE attempt_id=? AND provider=? AND account=? AND action_class=?`, a.ID, a.Provider, a.Account, o.Channel)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return restoredWorkInvalid()
		}
		summary.Attempts++
	}
	return nil
}

func validateRestoredAttempt(ctx context.Context, tx *sql.Tx, a Attempt, o Operation) error {
	var repo, resRepo, resPull int64
	var provider, account, class string
	if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, o.ProjectID).Scan(&repo); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT repository_id,pull_number,provider,account,action_class FROM review_execution_reservations WHERE attempt_id=?`, a.ID).Scan(&resRepo, &resPull, &provider, &account, &class); err != nil {
		return err
	}
	if repo <= 0 || resRepo != repo || resPull != o.PullNumber || provider != a.Provider || account != a.Account || class != o.Channel {
		return restoredWorkInvalid()
	}
	if _, err := sourceConfiguration(ctx, tx, o.ProjectID, o.ConfigurationRevision); err != nil {
		return err
	}
	var anchorJSON string
	if err := tx.QueryRowContext(ctx, `SELECT anchor_json FROM review_execution_attempts WHERE id=?`, a.ID).Scan(&anchorJSON); err != nil {
		return err
	}
	if anchorJSON != encoded(a.Anchor) {
		return restoredWorkInvalid()
	}
	var firstState, firstReason, firstExternal, currentExternal, currentAt string
	var count, maxRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT state,reason,external_id FROM review_execution_transitions WHERE attempt_id=? AND revision=1`, a.ID).Scan(&firstState, &firstReason, &firstExternal); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT external_id,created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision).Scan(&currentExternal, &currentAt); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*),max(revision) FROM review_execution_transitions WHERE attempt_id=?`, a.ID).Scan(&count, &maxRevision); err != nil {
		return err
	}
	if firstState != "preparing" || firstReason != "preparing" || firstExternal != "" || currentExternal != a.ExternalID || count != a.Revision || maxRevision != a.Revision {
		return restoredWorkInvalid()
	}
	if a.State == "preparing" {
		if a.Revision != 1 || a.Anchor != nil || a.Intent != "" || a.ExternalID != "" {
			return restoredWorkInvalid()
		}
	} else {
		if a.Anchor == nil || !validEvidenceAnchor(*a.Anchor) || a.Anchor.RepositoryID != repo || a.Anchor.PullNumber != o.PullNumber || a.Anchor.CandidateSHA != o.HeadSHA {
			return restoredWorkInvalid()
		}
		if o.Channel == "hosted" {
			if _, _, err := readHostedPostingAttempt(ctx, tx, o.ProjectID, a.ID); err != nil {
				return err
			}
			if err := validateSettlementAdmission(ctx, tx, a, o, currentAt); err != nil {
				return err
			}
		} else {
			var state, reason, external, stamp string
			if err := tx.QueryRowContext(ctx, `SELECT state,reason,external_id,created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=2`, a.ID).Scan(&state, &reason, &external, &stamp); err != nil {
				return err
			}
			admission, ok := attemptRecordedTime(stamp)
			current, _ := attemptRecordedTime(currentAt)
			created, _ := attemptRecordedTime(o.CreatedAt)
			if a.Revision < 2 || a.Intent != "" || state != "active" || reason != "admitted" || external != "" || !ok || admission.Before(created) || admission.After(current) {
				return restoredWorkInvalid()
			}
		}
	}
	var query bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_hosted_allowance_queries WHERE attempt_id=?)`, a.ID).Scan(&query); err != nil {
		return err
	}
	if query {
		if _, err := readHostedQuery(ctx, tx, a, o); err != nil {
			return err
		}
	}
	return nil
}

func pauseRestoredConfigurations(ctx context.Context, tx *sql.Tx, summary *RestoredWorkSummary) error {
	projects, err := restoredSelected(ctx, tx, `SELECT project_id,project_id FROM review_execution_config GROUP BY project_id ORDER BY project_id LIMIT 10001`)
	if err != nil {
		return err
	}
	for _, p := range projects {
		_, revision, err := readConfiguration(ctx, tx, p.project)
		if err != nil {
			return err
		}
		c, err := sourceConfiguration(ctx, tx, p.project, revision)
		if err != nil {
			return err
		}
		if !identifier.MatchString(p.project) || revision < 1 || revision == math.MaxInt64 || *c.ExpectedRevision != revision-1 {
			return restoredWorkInvalid()
		}
		if c.Paused {
			continue
		}
		var principal string
		if err = tx.QueryRowContext(ctx, `SELECT c.principal_id FROM metadata m JOIN credentials c ON c.id=m.value WHERE m.key='restore:activation_credential_id'`).Scan(&principal); err != nil {
			return restoredWorkInvalid()
		}
		if !identifier.MatchString(principal) {
			return restoredWorkInvalid()
		}
		c.Paused = true
		c.ExpectedRevision = &revision
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_execution_config(project_id,revision,body_json) VALUES(?,?,?)`, p.project, revision+1, encoded(c)); err != nil {
			return err
		}
		if err = core.AppendEvent(ctx, tx, core.Scope{ProjectID: p.project, PrincipalID: principal}, "reviews.configured", p.project, revision+1, map[string]any{"revision": revision + 1, "paused": true}); err != nil {
			return err
		}
		summary.PausedProjects++
	}
	return nil
}

func restoredStamp(created, updated string, at time.Time) (string, error) {
	first, err := time.Parse(time.RFC3339Nano, created)
	last, err2 := time.Parse(time.RFC3339Nano, updated)
	if err != nil || err2 != nil || first.IsZero() || last.IsZero() {
		return "", restoredWorkInvalid()
	}
	if at.Before(first) {
		at = first
	}
	if at.Before(last) {
		at = last
	}
	return at.UTC().Format("2006-01-02T15:04:05.000000000Z"), nil
}
func abandonRestoredSources(ctx context.Context, tx *sql.Tx, at time.Time, summary *RestoredWorkSummary) error {
	selected, err := restoredSelected(ctx, tx, `SELECT project_id,id FROM review_source_refreshes WHERE state IN ('queued','running') ORDER BY id LIMIT 10001`)
	if err != nil {
		return err
	}
	for _, id := range selected {
		r, err := readSourceRefresh(ctx, tx, id.project, id.id)
		if err != nil {
			return err
		}
		stamp, err := restoredStamp(r.CreatedAt, r.UpdatedAt, at)
		if err != nil {
			return err
		}
		if r.Revision == math.MaxInt64 || r.Observation != nil || r.CI != nil || r.ReviewState != nil || r.ciObservation != nil || r.reviewObservation != nil {
			return restoredWorkInvalid()
		}
		res, err := tx.ExecContext(ctx, `UPDATE review_source_refreshes SET state='held',reason='restore_reconciled',revision=revision+1,updated_at=? WHERE id=? AND project_id=? AND revision=? AND state=?`, stamp, r.ID, r.ProjectID, r.Revision, r.State)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return restoredWorkInvalid()
		}
		if err = core.AppendEvent(ctx, tx, core.Scope{ProjectID: r.ProjectID, PrincipalID: r.actor, WorkerID: r.worker}, "review.source_refresh_updated", r.ID, r.Revision+1, map[string]any{"state": "held", "reason": "restore_reconciled"}); err != nil {
			return err
		}
		summary.SourceRefreshes++
	}
	return nil
}
func abandonRestoredPublications(ctx context.Context, tx *sql.Tx, at time.Time, summary *RestoredWorkSummary) error {
	selected, err := restoredSelected(ctx, tx, `SELECT project_id,id FROM review_checkpoint_publications WHERE state NOT IN ('published','cancelled') ORDER BY id LIMIT 10001`)
	if err != nil {
		return err
	}
	for _, id := range selected {
		r, err := readCheckpoint(ctx, tx, id.project, id.id)
		if err != nil {
			return err
		}
		stamp, err := restoredStamp(r.View.CreatedAt, r.View.UpdatedAt, at)
		if err != nil {
			return err
		}
		if r.View.Revision == math.MaxInt64 {
			return restoredWorkInvalid()
		}
		previous := r.View.Revision
		r.View.Revision++
		r.View.State = "cancelled"
		r.View.Reason = "restore_reconciled"
		r.View.UpdatedAt = stamp
		if err = persistCheckpoint(ctx, tx, r, previous); err != nil {
			return err
		}
		summary.Publications++
	}
	return nil
}
