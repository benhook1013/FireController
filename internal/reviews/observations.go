package reviews

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
)

const (
	maxQueuePulls       = 100
	maxPullRuns         = 100
	maxSnapshotRuns     = 200
	maxPullAttempts     = 100
	maxSnapshotAttempts = 200
	maxRunFindings      = 100
	maxSnapshotFindings = 1000
	maxObservationBytes = 256 << 10
)

func validateObservation(req *ObservationRequest) error {
	if req.ExpectedRevision == nil || *req.ExpectedRevision < 0 || req.RepositoryID <= 0 || !validText(req.Source, 128, true) {
		return validationError("Provide an expected revision, repository identity and bounded source label.")
	}
	observed, err := parseTimestamp(req.ObservedAt, true)
	if err != nil || observed.After(time.Now().Add(5*time.Minute)) {
		return validationError("observed_at must be a valid RFC3339Nano timestamp no more than five minutes in the future.")
	}
	if req.Queue == nil {
		req.Queue = []PullInput{}
	}
	if req.Retired == nil {
		req.Retired = []RetiredPull{}
	}
	if len(req.Queue) > maxQueuePulls || len(req.Retired) > maxQueuePulls {
		return validationError("The observation exceeds the bounded queue or retirement count.")
	}
	var runCount, attemptCount, findingCount int
	pullNumbers := make(map[int64]struct{}, len(req.Queue))
	runIDs := make(map[string]struct{})
	attemptIDs := make(map[string]struct{})
	for pullIndex := range req.Queue {
		pull := &req.Queue[pullIndex]
		if pull.Number < 1 || pull.Number > 2147483647 {
			return validationError("Pull request numbers must be positive 32-bit values.")
		}
		if _, exists := pullNumbers[pull.Number]; exists {
			return validationError("Pull request numbers must be unique within an observation.")
		}
		pullNumbers[pull.Number] = struct{}{}
		if !validText(pull.Title, 500, true) || !oneOf(pull.State, "open", "merged", "closed", "unknown") ||
			!validSHA(pull.HeadSHA) || !validText(pull.BaseRef, 256, false) || !validSHA(pull.BaseSHA) ||
			!oneOf(pull.CIState, "unknown", "pending", "success", "failure") ||
			!oneOf(pull.Mergeability, "unknown", "mergeable", "conflicting") {
			return validationError("A pull request contains invalid identity, state or summary fields.")
		}
		if pull.Runs == nil {
			pull.Runs = []RunInput{}
		}
		if pull.Attempts == nil {
			pull.Attempts = []Attempt{}
		}
		if len(pull.Runs) > maxPullRuns || len(pull.Attempts) > maxPullAttempts {
			return validationError("A pull request exceeds its bounded run or attempt count.")
		}
		runCount += len(pull.Runs)
		attemptCount += len(pull.Attempts)
		if runCount > maxSnapshotRuns || attemptCount > maxSnapshotAttempts {
			return validationError("The observation exceeds the bounded run or attempt count.")
		}
		localRuns := make(map[string]struct{}, len(pull.Runs))
		for runIndex := range pull.Runs {
			run := &pull.Runs[runIndex]
			if !validText(run.SourceRunID, 256, true) || !oneOf(run.Channel, "cli", "hosted", "manual", "subagent") ||
				!validSHA(run.HeadSHA) || !validText(run.Reviewer, 128, false) || !validText(run.Scope, 128, true) ||
				!oneOf(run.Outcome, "completed", "active", "prepared", "posting", "failed", "cancelled", "rate_limited", "uncertain", "unknown") ||
				run.Found < 0 || run.Accepted < 0 || run.Routed < 0 || run.Accepted > run.Found-run.Routed ||
				(run.Complete && int64(len(run.Findings)) != run.Found) || int64(len(run.Findings)) > run.Found {
				return validationError("A review run contains invalid identity, state, count or finding fields.")
			}
			if _, exists := runIDs[run.SourceRunID]; exists {
				return validationError("source_run_id values must be unique within the observation source.")
			}
			runIDs[run.SourceRunID] = struct{}{}
			localRuns[run.SourceRunID] = struct{}{}
			if run.CoverageLimits == nil {
				run.CoverageLimits = []string{}
			}
			if len(run.CoverageLimits) > 16 {
				return validationError("A review run exceeds its bounded coverage limit count.")
			}
			for _, coverageLimit := range run.CoverageLimits {
				if !validText(coverageLimit, 1024, true) {
					return validationError("A review run contains an invalid coverage limit.")
				}
			}
			if run.Findings == nil {
				run.Findings = []Finding{}
			}
			if len(run.Findings) > maxRunFindings {
				return validationError("A review run exceeds its bounded finding count.")
			}
			findingCount += len(run.Findings)
			if findingCount > maxSnapshotFindings {
				return validationError("The observation exceeds the bounded finding count.")
			}
			if err := validateRunTimes(*run); err != nil {
				return err
			}
			seenFindings := make(map[string]struct{}, len(run.Findings))
			for _, finding := range run.Findings {
				if !validText(finding.SourceFindingID, 256, true) || !validText(finding.Title, 500, true) ||
					!validText(finding.Detail, 16<<10, false) || !oneOf(finding.Severity, "info", "low", "medium", "high", "critical", "unknown") ||
					!oneOf(finding.Disposition, "unadjudicated", "accepted", "routed", "rejected") ||
					!validText(finding.DecisionReason, 2<<10, false) || !validText(finding.Actor, 128, false) ||
					!validSHA(finding.FixSHA) || !validText(finding.ProofNote, 2<<10, false) {
					return validationError("A finding contains invalid identity, content or disposition fields.")
				}
				if _, exists := seenFindings[finding.SourceFindingID]; exists {
					return validationError("source_finding_id values must be unique within a run.")
				}
				seenFindings[finding.SourceFindingID] = struct{}{}
				if (finding.FixSHA != "" || finding.ProofNote != "") && finding.Disposition != "accepted" || finding.ResolvedAt != "" && finding.FixSHA == "" {
					return validationError("Fix evidence must belong to an accepted finding and include its fix SHA.")
				}
				if _, err := parseTimestamp(finding.DecidedAt, false); err != nil {
					return validationError("Finding decision timestamps must be empty or RFC3339Nano.")
				}
				if _, err := parseTimestamp(finding.ResolvedAt, false); err != nil {
					return validationError("Finding resolution timestamps must be empty or RFC3339Nano.")
				}
			}
		}
		for _, attempt := range pull.Attempts {
			if !validText(attempt.SourceAttemptID, 256, true) || !oneOf(attempt.Channel, "cli", "hosted", "manual", "subagent") ||
				!validSHA(attempt.HeadSHA) || !oneOf(attempt.State, "prepared", "preparing", "held", "posting", "active", "completed", "failed", "cancelled", "rate_limited", "uncertain", "unknown") ||
				(attempt.DurationSeconds != nil && *attempt.DurationSeconds < 0) || !validText(attempt.SourceRunID, 256, false) ||
				!validText(attempt.TriggerID, 128, false) || !validText(attempt.ProviderReviewID, 128, false) {
				return validationError("A review attempt contains invalid identity, state or time fields.")
			}
			if _, exists := attemptIDs[attempt.SourceAttemptID]; exists {
				return validationError("source_attempt_id values must be unique within the observation source.")
			}
			attemptIDs[attempt.SourceAttemptID] = struct{}{}
			if attempt.SourceRunID != "" {
				if _, exists := localRuns[attempt.SourceRunID]; !exists {
					return validationError("An attempt source_run_id must refer to a run on the same pull request.")
				}
			}
			if err := validateAttemptTimes(attempt); err != nil {
				return err
			}
		}
	}
	retired := make(map[int64]struct{}, len(req.Retired))
	for _, entry := range req.Retired {
		if entry.Number < 1 || entry.Number > 2147483647 || !oneOf(entry.Reason, "superseded", "defunct") {
			return validationError("Retired pull requests require a valid number and reason.")
		}
		if _, exists := retired[entry.Number]; exists {
			return validationError("Retired pull request numbers must be unique.")
		}
		if _, stillQueued := pullNumbers[entry.Number]; stillQueued {
			return validationError("A pull request cannot be both queued and retired.")
		}
		retired[entry.Number] = struct{}{}
	}
	return nil
}

func validateRunTimes(run RunInput) error {
	start, startErr := parseTimestamp(run.StartedAt, false)
	finish, finishErr := parseTimestamp(run.FinishedAt, false)
	if startErr != nil || finishErr != nil {
		return validationError("Review run timestamps must be empty or RFC3339Nano.")
	}
	if run.Outcome == "completed" {
		if run.StartedAt == "" || run.FinishedAt == "" || finish.Before(start) {
			return validationError("Completed review runs require ordered start and finish timestamps.")
		}
	} else if run.Complete && (run.StartedAt == "" || run.FinishedAt == "" || finish.Before(start)) {
		return validationError("Complete review runs require ordered start and finish timestamps.")
	}
	return nil
}

func validateAttemptTimes(attempt Attempt) error {
	start, startErr := parseTimestamp(attempt.StartedAt, false)
	finish, finishErr := parseTimestamp(attempt.FinishedAt, false)
	if startErr != nil || finishErr != nil || (attempt.FinishedAt != "" && attempt.StartedAt != "" && finish.Before(start)) {
		return validationError("Review attempt timestamps must be valid and ordered.")
	}
	if attempt.State == "completed" && (attempt.StartedAt == "" || attempt.FinishedAt == "") {
		return validationError("Completed attempts require start and finish timestamps.")
	}
	return nil
}

func validText(value string, maxBytes int, nonblank bool) bool {
	if len(value) > maxBytes || !utf8.ValidString(value) || nonblank && strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if r == 0x7f || r < 0x20 && r != '\t' && r != '\r' && r != '\n' {
			return false
		}
	}
	return true
}

func validSHA(value string) bool {
	if value == "" {
		return true
	}
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func oneOf(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

func parseTimestamp(value string, required bool) (time.Time, error) {
	if value == "" {
		if required {
			return time.Time{}, errors.New("timestamp required")
		}
		return time.Time{}, nil
	}
	if !validText(value, 64, true) {
		return time.Time{}, errors.New("invalid timestamp")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed, nil
}

func validationError(message string) error {
	return core.Fail("validation_failed", message, http.StatusBadRequest)
}

func runOpaqueID(projectID, source, sourceRunID string) string {
	sum := sha256.Sum256([]byte(projectID + "\x00" + source + "\x00" + sourceRunID))
	return "review_run_" + hex.EncodeToString(sum[:])[:32]
}

func appendObservation(store *core.Store, w http.ResponseWriter, r *http.Request) {
	scope, err := store.AdminScope(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var req ObservationRequest
	if err := core.ReadJSON(w, r, &req); err != nil {
		core.WriteError(w, err)
		return
	}
	if err := validateObservation(&req); err != nil {
		core.WriteError(w, err)
		return
	}
	key, err := core.IdempotencyKey(r, "validation_failed")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	requestBytes, err := json.Marshal(req)
	if err != nil || len(requestBytes) > maxObservationBytes {
		core.WriteError(w, validationError("The normalized observation exceeds 256 KiB."))
		return
	}
	payloadHash := sha256.Sum256(requestBytes)
	result, err := store.Mutate(r.Context(), scope, core.Mutation{
		Operation: "reviews.observe", Target: "workspace", Key: key, Request: req,
	}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		var repositoryID int64
		if err := tx.QueryRowContext(ctx, "SELECT github_repository_id FROM projects WHERE id=?", scope.ProjectID).Scan(&repositoryID); err != nil {
			return nil, err
		}
		if repositoryID != req.RepositoryID {
			return nil, validationError("repository_id does not match the configured project repository.")
		}
		currentRevision, currentSnapshot, currentObserved := int64(0), "", ""
		err := tx.QueryRowContext(ctx, `SELECT w.revision,w.snapshot_id,s.observed_at
			FROM review_workspace w JOIN review_snapshots s ON s.project_id=w.project_id AND s.snapshot_id=w.snapshot_id
			WHERE w.project_id=?`, scope.ProjectID).Scan(&currentRevision, &currentSnapshot, &currentObserved)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if errors.Is(err, sql.ErrNoRows) {
			currentRevision, currentSnapshot, currentObserved = 0, "", ""
		}
		if *req.ExpectedRevision != currentRevision {
			return nil, &core.Error{Code: "revision_conflict", Message: "The review observation revision changed.", Status: http.StatusConflict, Details: map[string]int64{"current_revision": currentRevision}}
		}
		if currentRevision == int64(^uint64(0)>>1) {
			return nil, core.Fail("revision_exhausted", "The review workspace revision cannot advance further.", http.StatusConflict)
		}
		observed, _ := parseTimestamp(req.ObservedAt, true)
		if currentObserved != "" {
			previous, parseErr := parseTimestamp(currentObserved, true)
			if parseErr != nil {
				return nil, core.Fail("storage_unavailable", "The stored review observation timestamp is invalid.", http.StatusInternalServerError)
			}
			if observed.Before(previous) {
				return nil, core.Fail("stale_observation", "The observation is older than the current project snapshot.", http.StatusConflict)
			}
		}
		previousPulls, err := pullNumbersForSnapshot(ctx, tx, scope.ProjectID, currentSnapshot)
		if err != nil {
			return nil, err
		}
		if err := validateRetirements(previousPulls, req.Queue, req.Retired); err != nil {
			return nil, err
		}
		if err := validateFinalizedCounts(ctx, tx, scope.ProjectID, req.Source, req.Queue); err != nil {
			return nil, err
		}
		nextRevision := currentRevision + 1
		snapshotID := core.NewID("review_snapshot")
		recordedAt := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_snapshots
			(project_id,snapshot_id,revision,repository_id,source,observed_at,recorded_at,payload_sha256)
			VALUES(?,?,?,?,?,?,?,?)`, scope.ProjectID, snapshotID, nextRevision, req.RepositoryID, req.Source,
			req.ObservedAt, recordedAt, hex.EncodeToString(payloadHash[:])); err != nil {
			return nil, err
		}
		for i, pull := range req.Queue {
			if err := storePull(ctx, tx, scope.ProjectID, snapshotID, req.Source, i, pull); err != nil {
				return nil, err
			}
		}
		for i, retired := range req.Retired {
			if _, err := tx.ExecContext(ctx, `INSERT INTO review_retired_pulls(project_id,snapshot_id,position,number,reason) VALUES(?,?,?,?,?)`,
				scope.ProjectID, snapshotID, i, retired.Number, retired.Reason); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_workspace(project_id,revision,snapshot_id) VALUES(?,?,?)
			ON CONFLICT(project_id) DO UPDATE SET revision=excluded.revision,snapshot_id=excluded.snapshot_id`,
			scope.ProjectID, nextRevision, snapshotID); err != nil {
			return nil, err
		}
		if err := core.AppendEvent(ctx, tx, scope, "review.observed", snapshotID, nextRevision,
			map[string]any{"snapshot_id": snapshotID, "queue_count": len(req.Queue)}); err != nil {
			return nil, err
		}
		return Receipt{ID: snapshotID, Revision: nextRevision, Outcome: "observed"}, nil
	})
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusCreated, json.RawMessage(result))
}

func pullNumbersForSnapshot(ctx context.Context, tx *sql.Tx, projectID, snapshotID string) ([]int64, error) {
	if snapshotID == "" {
		return []int64{}, nil
	}
	rows, err := tx.QueryContext(ctx, "SELECT number FROM review_pulls WHERE project_id=? AND snapshot_id=? ORDER BY position", projectID, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]int64, 0)
	for rows.Next() {
		var number int64
		if err := rows.Scan(&number); err != nil {
			return nil, err
		}
		out = append(out, number)
	}
	return out, rows.Err()
}

func validateRetirements(previous []int64, queue []PullInput, retired []RetiredPull) error {
	queued := make(map[int64]struct{}, len(queue))
	for _, pull := range queue {
		queued[pull.Number] = struct{}{}
	}
	retiredSet := make(map[int64]string, len(retired))
	for _, item := range retired {
		retiredSet[item.Number] = item.Reason
	}
	previousSet := make(map[int64]struct{}, len(previous))
	for _, number := range previous {
		previousSet[number] = struct{}{}
		_, remains := queued[number]
		_, noted := retiredSet[number]
		if !remains && !noted {
			return validationError("Every removed queue entry requires an explicit retirement reason.")
		}
	}
	for number := range retiredSet {
		if _, existed := previousSet[number]; !existed {
			return validationError("A retirement must refer to an entry in the previous queue snapshot.")
		}
		if _, remains := queued[number]; remains {
			return validationError("A pull request cannot be both queued and retired.")
		}
	}
	return nil
}

const finalizedRunQuery = `SELECT snapshot_id,pull_number,channel,head_sha,reviewer,scope,found,accepted,routed,id FROM review_runs
 WHERE project_id=? AND source=? AND source_run_id=? AND finalized=1 ORDER BY snapshot_revision DESC LIMIT 1`

func validateFinalizedCounts(ctx context.Context, tx *sql.Tx, projectID, source string, queue []PullInput) error {
	for _, pull := range queue {
		for _, run := range pull.Runs {
			var snapshotID, channel, headSHA, reviewer, scope, runID string
			var pullNumber, found, accepted, routed int64
			err := tx.QueryRowContext(ctx, finalizedRunQuery, projectID, source, run.SourceRunID).Scan(&snapshotID, &pullNumber, &channel, &headSHA, &reviewer, &scope, &found, &accepted, &routed, &runID)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if found != run.Found || accepted != run.Accepted || routed != run.Routed || !run.Finalized ||
				pullNumber != pull.Number || channel != run.Channel || headSHA != run.HeadSHA || reviewer != run.Reviewer || scope != run.Scope {
				return core.Fail("observation_conflict", "Finalized source run identity and counts cannot be changed or reopened.", http.StatusConflict)
			}
			// Every admitted finalized copy retains all previously known findings.
			// The latest finalized copy therefore carries their cumulative identity
			// and text, even when intervening snapshots omitted the run entirely.
			if err := validateFinalizedFindings(ctx, tx, projectID, snapshotID, pullNumber, runID, run.Findings); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateFinalizedFindings(ctx context.Context, tx *sql.Tx, projectID, snapshotID string, pullNumber int64, runID string, current []Finding) error {
	currentByID := make(map[string]Finding, len(current))
	for _, finding := range current {
		currentByID[finding.SourceFindingID] = finding
	}
	rows, err := tx.QueryContext(ctx, `SELECT source_finding_id,title,detail FROM review_findings
		WHERE project_id=? AND snapshot_id=? AND pull_number=? AND run_id=?`, projectID, snapshotID, pullNumber, runID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, title, detail string
		if err := rows.Scan(&id, &title, &detail); err != nil {
			return err
		}
		finding, exists := currentByID[id]
		if !exists || finding.Title != title || finding.Detail != detail {
			return core.Fail("observation_conflict", "Finalized source finding identities and text cannot be replaced or removed.", http.StatusConflict)
		}
	}
	return rows.Err()
}

func storePull(ctx context.Context, tx *sql.Tx, projectID, snapshotID, source string, position int, pull PullInput) error {
	var snapshotRevision int64
	if len(pull.Runs) > 0 {
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM review_snapshots WHERE project_id=? AND snapshot_id=?`, projectID, snapshotID).Scan(&snapshotRevision); err != nil {
			return err
		}
	}
	recent := make([]RunSummary, 0, 3)
	for i := len(pull.Runs) - 1; i >= 0 && len(recent) < 3; i-- {
		recent = append(recent, summarizeRun(projectID, source, pull.Number, pull.Runs[i]))
	}
	latest := latestAttempts(pull.Attempts)
	recentJSON, err := json.Marshal(recent)
	if err != nil {
		return fmt.Errorf("encode compact run projection: %w", err)
	}
	latestJSON, err := json.Marshal(latest)
	if err != nil {
		return fmt.Errorf("encode compact attempt projection: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO review_pulls
		(project_id,snapshot_id,position,number,title,state,head_sha,base_ref,base_sha,ci_state,mergeability,complete,run_count,attempt_count,recent_runs_json,latest_attempts_json)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, projectID, snapshotID, position, pull.Number, pull.Title, pull.State,
		pull.HeadSHA, pull.BaseRef, pull.BaseSHA, pull.CIState, pull.Mergeability, pull.Complete, len(pull.Runs), len(pull.Attempts),
		string(recentJSON), string(latestJSON)); err != nil {
		return err
	}
	for order, run := range pull.Runs {
		id := runOpaqueID(projectID, source, run.SourceRunID)
		fixed := int64(0)
		for _, finding := range run.Findings {
			if finding.FixSHA != "" {
				fixed++
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_runs
			(project_id,snapshot_id,snapshot_revision,pull_number,run_order,id,source,source_run_id,channel,head_sha,reviewer,scope,outcome,attributable,complete,started_at,finished_at,found,accepted,routed,finalized,fixed,coverage_limits_json)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, projectID, snapshotID, snapshotRevision, pull.Number, order, id, source,
			run.SourceRunID, run.Channel, run.HeadSHA, run.Reviewer, run.Scope, run.Outcome, run.Attributable, run.Complete,
			run.StartedAt, run.FinishedAt, run.Found, run.Accepted, run.Routed, run.Finalized, fixed, mustJSON(run.CoverageLimits)); err != nil {
			return err
		}
		for index, finding := range run.Findings {
			if _, err := tx.ExecContext(ctx, `INSERT INTO review_findings
				(project_id,snapshot_id,pull_number,run_id,position,source_finding_id,title,detail,severity,disposition,decision_reason,actor,decided_at,fix_sha,proof_note,resolved_at)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, projectID, snapshotID, pull.Number, id, index,
				finding.SourceFindingID, finding.Title, finding.Detail, finding.Severity, finding.Disposition,
				finding.DecisionReason, finding.Actor, finding.DecidedAt, finding.FixSHA, finding.ProofNote, finding.ResolvedAt); err != nil {
				return err
			}
		}
	}
	for index, attempt := range pull.Attempts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_attempts
			(project_id,snapshot_id,pull_number,position,source,source_attempt_id,channel,head_sha,state,started_at,finished_at,duration_seconds,source_run_id,trigger_id,provider_review_id)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, projectID, snapshotID, pull.Number, index, source, attempt.SourceAttemptID,
			attempt.Channel, attempt.HeadSHA, attempt.State, attempt.StartedAt, attempt.FinishedAt, attempt.DurationSeconds,
			attempt.SourceRunID, attempt.TriggerID, attempt.ProviderReviewID); err != nil {
			return err
		}
	}
	return nil
}

func summarizeRun(projectID, source string, pullNumber int64, run RunInput) RunSummary {
	fixed := int64(0)
	for _, finding := range run.Findings {
		if finding.FixSHA != "" {
			fixed++
		}
	}
	return RunSummary{Source: source, ID: runOpaqueID(projectID, source, run.SourceRunID), PullNumber: pullNumber,
		SourceRunID: run.SourceRunID, Channel: run.Channel, HeadSHA: run.HeadSHA, Reviewer: run.Reviewer,
		Scope: run.Scope, Outcome: run.Outcome, Attributable: run.Attributable, Complete: run.Complete,
		StartedAt: run.StartedAt, FinishedAt: run.FinishedAt, Found: run.Found, Accepted: run.Accepted,
		Routed: run.Routed, Finalized: run.Finalized, Fixed: fixed, CoverageLimits: run.CoverageLimits}
}

func latestAttempts(attempts []Attempt) []AttemptSummary {
	byChannel := make(map[string]Attempt)
	order := map[string]int{}
	for index, item := range attempts {
		old, exists := byChannel[item.Channel]
		candidate, previous := attemptOrder(item), attemptOrder(old)
		if !exists || candidate.After(previous) || candidate.Equal(previous) && index > order[item.Channel] {
			byChannel[item.Channel] = item
			order[item.Channel] = index
		}
	}
	result := make([]AttemptSummary, 0, len(byChannel))
	for _, channel := range []string{"hosted", "cli", "manual", "subagent"} {
		if item, ok := byChannel[channel]; ok {
			result = append(result, AttemptSummary(item))
		}
	}
	return result
}

func attemptOrder(item Attempt) time.Time {
	start, _ := parseTimestamp(item.StartedAt, false)
	finish, _ := parseTimestamp(item.FinishedAt, false)
	if finish.After(start) {
		return finish
	}
	return start
}

func mustJSON(value any) string { data, _ := json.Marshal(value); return string(data) }
