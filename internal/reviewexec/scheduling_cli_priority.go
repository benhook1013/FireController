package reviewexec

import (
	"context"
	"database/sql"
	"slices"
	"strings"
)

const preferredCLIRepository = "benhook1013/firemud"

type pendingCandidateID struct{ project, operation string }

// Prefer the exact configured repository identity, never a mutable project name.
// No running review is preempted and no wait deadline or operation is replaced.
func (e *Engine) preferredCLIQueued(ctx context.Context, tx *sql.Tx, o Operation, c Configuration) (bool, error) {
	if o.Channel != "cli" || o.State != "queued" {
		return false, nil
	}
	capability, installed := e.capabilities[c.CapabilityID]
	if !installed || capability.Fingerprint != o.CapabilityFingerprint {
		return false, nil
	}
	var repository string
	if err := tx.QueryRowContext(ctx, `SELECT repository FROM projects WHERE id=?`, o.ProjectID).Scan(&repository); err != nil {
		return false, err
	}
	if strings.EqualFold(repository, preferredCLIRepository) {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT o.project_id,o.id FROM review_operations o JOIN projects p ON p.id=o.project_id WHERE o.channel='cli' AND o.state='queued' AND o.reason<>'cli_draft_wait' AND lower(p.repository)=? AND o.project_id<>? ORDER BY o.created_at,o.id LIMIT 101`, preferredCLIRepository, o.ProjectID)
	if err != nil {
		return false, err
	}
	var ids []pendingCandidateID
	for rows.Next() {
		var id pendingCandidateID
		if err = rows.Scan(&id.project, &id.operation); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err != nil {
		return false, err
	}
	if closeErr != nil {
		return false, closeErr
	}
candidates:
	for _, id := range ids {
		earlier, err := readOperation(ctx, tx, id.project, id.operation)
		if err != nil {
			if isStoredUnavailable(err, storedOperationIdentity) || isRetainedOperationDecodeError(err) || isWaitUnavailable(err) || automaticProjectRefused(err) || isStoredUnavailable(err, storedConfigurationHistory) {
				continue candidates
			}
			return false, err
		}
		if earlier.Wait != nil && earlier.Wait.Paused {
			continue
		}
		priorConfig, _, err := readConfiguration(ctx, tx, id.project)
		if err != nil {
			if automaticProjectRefused(err) || isStoredUnavailable(err, storedConfigurationHistory) {
				continue candidates
			}
			return false, err
		}
		priorCapability, ok := e.capabilities[priorConfig.CapabilityID]
		if !ok || priorCapability.Fingerprint != earlier.CapabilityFingerprint || priorCapability.Provider != capability.Provider || priorCapability.Account != capability.Account {
			continue
		}
		// An absent wait row is not proof that the candidate is fresh. A
		// detached cooldown must still bind the original retry comparison.
		if err := rejectMissingRetryWait(ctx, tx, earlier); err != nil {
			if isWaitUnavailable(err) {
				continue candidates
			}
			return false, err
		}
		if earlier.Wait != nil && earlier.Wait.RetryNotBefore != "" && e.sampleExecutionTime().Before(mustWaitTime(earlier.Wait.RetryNotBefore)) {
			continue
		}
		valid, err := e.hostedPriorityBinding(ctx, tx, earlier)
		if err != nil {
			if automaticProjectRefused(err) || isStoredUnavailable(err, storedConfigurationHistory) || isStoredUnavailable(err, storedRoundHistory) {
				continue candidates
			}
			return false, err
		}
		if valid {
			return true, nil
		}
	}
	return e.preferredCLISelection(ctx, tx, o, capability)
}

// A concurrently selected preferred project gets to finish its bounded source
// check before a competing account claim. Held/finished selections do not block.
// Match ordinary selection capacity: pending operations must be allowed to
// drain when the preferred selection cannot create its child operation.
func (e *Engine) preferredCLISelection(ctx context.Context, tx *sql.Tx, o Operation, capability Capability) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT s.project_id,s.id FROM review_selections s JOIN projects p ON p.id=s.project_id WHERE s.channel='cli' AND s.state IN('queued','selecting') AND lower(p.repository)=? AND s.project_id<>?
 AND (SELECT count(*) FROM review_operations WHERE state IN ('queued','preparing','posting','active','uncertain'))<?
 AND (SELECT count(*) FROM review_operations o WHERE o.project_id=s.project_id AND o.state IN ('queued','preparing','posting','active','uncertain'))<?
 ORDER BY s.created_at,s.id LIMIT 101`, preferredCLIRepository, o.ProjectID, requestGlobalCapacity, requestProjectCapacity)
	if err != nil {
		return false, err
	}
	ids, err := readPreferredSelectionIDs(rows)
	if err != nil {
		return false, err
	}
candidates:
	for _, id := range ids {
		r, err := readRun(ctx, tx, id.project, id.operation)
		if err != nil {
			if automaticProjectRefused(err) || isStoredUnavailable(err, storedConfigurationHistory) {
				continue candidates
			}
			return false, err
		}
		c, _, reason, err := e.runBinding(ctx, tx, r)
		if err != nil {
			if automaticProjectRefused(err) || isStoredUnavailable(err, storedConfigurationHistory) {
				continue candidates
			}
			return false, err
		}
		if reason != "" {
			continue
		}
		prior := e.capabilities[c.CapabilityID]
		if prior.Provider != capability.Provider || prior.Account != capability.Account {
			continue
		}
		queue := c.Queue
		attribution, err := automaticSelectionBinding(ctx, tx, r)
		if err != nil {
			if automaticProjectRefused(err) {
				continue candidates
			}
			return false, err
		}
		if attribution.revision != 0 {
			limit, err := e.automaticHostedQueuedPrefixChannel(ctx, tx, r.ProjectID, c, r.Channel)
			if err != nil {
				if automaticProjectRefused(err) {
					continue candidates
				}
				return false, err
			}
			queue = queue[:limit]
		}
		if r.ExpectedPullNumber != 0 {
			if !slices.Contains(queue, r.ExpectedPullNumber) {
				continue
			}
			queue = []int64{r.ExpectedPullNumber}
		}
		for _, pull := range queue {
			pause, err := readPullScheduling(ctx, tx, r.ProjectID, pull)
			if err != nil {
				if automaticProjectRefused(err) || isStoredUnavailable(err, storedConfigurationHistory) {
					continue candidates
				}
				return false, err
			}
			if !pause.Paused {
				return true, nil
			}
		}
	}
	return false, nil
}

// Read candidate keys before other lookups; database/sql can otherwise coerce
// a non-text value to a different key.
func readPreferredSelectionIDs(rows *sql.Rows) ([]pendingCandidateID, error) {
	var err error
	var ids []pendingCandidateID
	for rows.Next() {
		var id pendingCandidateID
		var rawID any
		if err = rows.Scan(&id.project, &rawID); err != nil {
			break
		}
		var text bool
		if id.operation, text = rawID.(string); !text {
			continue
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return ids, nil
}
