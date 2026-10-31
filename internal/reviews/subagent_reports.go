package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const subagentSource = "worker-subagent"

var subagentRunID = regexp.MustCompile(`^review_run_[a-f0-9]{32}$`)

func registerSubagentReports(mux *http.ServeMux, store *core.Store) {
	base := "/api/v1/projects/{project_id}/reviews/subagent-reports"
	mux.HandleFunc("POST "+base, recordSubagentReport(store))
	mux.HandleFunc("GET "+base+"/{run_id}", readSubagentReportHTTP(store))
	mux.HandleFunc("POST "+base+"/{run_id}/updates", updateSubagentReportHTTP(store))
}

func subagentScope(w http.ResponseWriter, r *http.Request, store *core.Store, permission string) (core.Scope, bool) {
	scope, err := store.Scope(r, permission)
	if err == nil {
		_, err = core.QueryKeys(r, "validation_failed")
	}
	if err != nil {
		core.WriteError(w, err)
		return core.Scope{}, false
	}
	return scope, true
}

// Required zero-valued fields (finding index zero, empty prior SHA) are distinct
// from omissions. Optional fields may be omitted but not explicitly null.
func subagentJSON(w http.ResponseWriter, r *http.Request, dst any, required []string, nested map[string][]string) error {
	var raw json.RawMessage
	if err := core.ReadJSON(w, r, &raw); err != nil {
		return err
	}
	if strictjson.Decode(raw, dst) != nil {
		return validationError("Use one strict subagent review object with declared fields.")
	}
	var value any
	if json.Unmarshal(raw, &value) != nil || !subagentNonNull(value) {
		return validationError("Subagent review fields cannot be null.")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return validationError("Use a subagent review object.")
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			return validationError("A required subagent review field is missing.")
		}
	}
	for list, keys := range nested {
		if body, ok := fields[list]; ok {
			var entries []map[string]json.RawMessage
			if json.Unmarshal(body, &entries) != nil {
				return validationError("Use a list of subagent review items.")
			}
			for _, entry := range entries {
				for _, key := range keys {
					if _, ok := entry[key]; !ok {
						return validationError("A required subagent review item field is missing.")
					}
				}
			}
		}
	}
	return nil
}
func subagentNonNull(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case map[string]any:
		for _, item := range v {
			if !subagentNonNull(item) {
				return false
			}
		}
	case []any:
		for _, item := range v {
			if !subagentNonNull(item) {
				return false
			}
		}
	}
	return true
}
func fullSubagentSHA(value string) bool {
	return len(value) == 40 && strings.Trim(value, "0123456789abcdef") == ""
}

func subagentRun(in SubagentRecordRequest, sourceID, actor, at string) (RunInput, error) {
	run := RunInput{SourceRunID: sourceID, Channel: "subagent", HeadSHA: in.HeadSHA, Reviewer: in.Reviewer, Scope: in.Scope, Outcome: "completed", Attributable: true, Complete: true, StartedAt: in.StartedAt, FinishedAt: in.FinishedAt, Found: int64(len(in.Findings)), Finalized: in.Finalized, CoverageLimits: in.CoverageLimits, Findings: make([]Finding, len(in.Findings))}
	if in.PullNumber < 1 || in.PullNumber > 2147483647 || !fullSubagentSHA(in.HeadSHA) || !nativeAdjudicationText(in.Reviewer, 128, true) || !nativeAdjudicationText(in.Scope, 128, true) || in.Findings == nil || len(in.Findings) > maxRunFindings {
		return run, validationError("Use a PR, full reviewed SHA, reviewer, scope and at most100 findings.")
	}
	for i, f := range in.Findings {
		disposition := f.Disposition
		if disposition == "" {
			disposition = "unadjudicated"
		}
		if !oneOf(disposition, "unadjudicated", "accepted", "rejected") || !nativeAdjudicationText(f.Reason, 2048, disposition != "unadjudicated") || disposition == "unadjudicated" && (f.Reason != "" || in.Finalized) {
			return run, validationError("Each decided finding requires its own reason; finalize after every decision.")
		}
		next := Finding{SourceFindingID: sourceID + ":finding:" + strconv.Itoa(i), Title: f.Title, Detail: f.Detail, Severity: f.Severity, Disposition: disposition, DecisionReason: f.Reason}
		if disposition != "unadjudicated" {
			next.Actor = actor
			next.DecidedAt = at
		}
		if disposition == "accepted" {
			run.Accepted++
		}
		run.Findings[i] = next
	}
	// Reuse the canonical channel history bounds, time ordering and detail rules.
	pull := PullInput{Number: in.PullNumber, Title: "Subagent review", State: "open", CIState: "unknown", Mergeability: "unknown", Runs: []RunInput{run}}
	if err := validateObservationContent([]PullInput{pull}, nil, false); err != nil {
		return run, err
	}
	return run, nil
}

func recordSubagentReport(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := subagentScope(w, r, store, "write")
		if !ok {
			return
		}
		var in SubagentRecordRequest
		if err := subagentJSON(w, r, &in, []string{"pull_number", "head_sha", "reviewer", "scope", "started_at", "finished_at", "findings"}, map[string][]string{"findings": {"title", "detail", "severity"}}); err != nil {
			core.WriteError(w, err)
			return
		}
		// Validate before entering the ordinary mutation budget; identities/times are
		// generated again only in the accepted transaction, not in caller arguments.
		if _, err := subagentRun(in, "validation", scope.PrincipalID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			core.WriteError(w, err)
			return
		}
		key, err := core.IdempotencyKey(r, "validation_failed")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		raw, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "reviews.subagent.record", Target: strconv.FormatInt(in.PullNumber, 10), Key: key, Request: in}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			meta, exists, err := selectSnapshot(ctx, tx, scope.ProjectID, "")
			if err != nil {
				return nil, err
			}
			if !exists {
				meta = snapshotMeta{}
			}
			var p PullInput
			var wasComplete bool
			p.Number = in.PullNumber
			err = tx.QueryRowContext(ctx, `SELECT title,state,head_sha,base_ref,base_sha,ci_state,mergeability,complete FROM review_pulls WHERE project_id=? AND snapshot_id=? AND number=?`, scope.ProjectID, meta.SnapshotID, in.PullNumber).Scan(&p.Title, &p.State, &p.HeadSHA, &p.BaseRef, &p.BaseSHA, &p.CIState, &p.Mergeability, &wasComplete)
			reviewOnly := errors.Is(err, sql.ErrNoRows)
			if reviewOnly {
				p = PullInput{Number: in.PullNumber, Title: fmt.Sprintf("PR #%d", in.PullNumber), State: "unknown", CIState: "unknown", Mergeability: "unknown"}
				err = nil
			}
			if err != nil {
				return nil, err
			}
			sourceID := core.NewID("subagent_review")
			run, err := subagentRun(in, sourceID, scope.PrincipalID, time.Now().UTC().Format(time.RFC3339Nano))
			if err != nil {
				return nil, err
			}
			p.Runs = []RunInput{run}
			req := TransferRequest{Version: 1, ExpectedRevision: &meta.Revision, RepositoryID: 0, Publisher: subagentSource, Groups: []TransferGroup{{Source: subagentSource, Pulls: []PullInput{p}}}}
			if err = tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, scope.ProjectID).Scan(&req.RepositoryID); err != nil {
				return nil, err
			}
			body, err := normalizeTransfer(&req)
			if err != nil {
				return nil, err
			}
			var nextPosition int
			if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position),-1)+1 FROM review_current_pulls WHERE project_id=?`, scope.ProjectID).Scan(&nextPosition); err != nil {
				return nil, err
			}
			selected, err := planTransferPull(ctx, tx, scope.ProjectID, meta.SnapshotID, p, nextPosition)
			if err != nil {
				return nil, err
			}
			id := runOpaqueID(scope.ProjectID, subagentSource, sourceID)
			version, _, err := retainedRunState(ctx, tx, scope.ProjectID, subagentSource, p.Number, run)
			if err != nil {
				return nil, err
			}
			if version != 0 {
				return nil, transferNativeConflict()
			}
			// Adding one independent review does not invalidate the PR's existing
			// metadata/completeness observation as a historical batch import may.
			if wasComplete {
				selected.body[7] = int64(1)
			}
			if reviewOnly {
				selected.body[13] = int64(0)
			}
			selected.runs[id] = selected.nextRun
			plan := transferPlan{revision: meta.Revision, snapshot: meta.SnapshotID, observed: meta.ObservedAt, pulls: map[int64]*transferPullPlan{p.Number: selected}, order: []int64{p.Number}}

			result, err := publishTransfer(ctx, tx, scope, req, body, plan)
			if err != nil {
				return nil, err
			}
			if err = checkNativeRunDetail(ctx, tx, scope.ProjectID, result.ID, id); err != nil {
				return nil, err
			}
			return SubagentReceipt{ID: id, State: "recorded", Revision: result.Revision}, nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusCreated, json.RawMessage(raw))
	}
}

func readSubagentReport(ctx context.Context, tx *sql.Tx, project, id string) (SubagentReport, error) {
	var out SubagentReport
	if !subagentRunID.MatchString(id) {
		return out, validationError("Use a complete review_run_ identity.")
	}
	meta, exists, err := selectSnapshot(ctx, tx, project, "")
	if err != nil {
		return out, err
	}
	if !exists {
		return out, notFound("The subagent review is unavailable in this project.")
	}
	detail, err := readRunDetail(ctx, tx, meta, id)
	if err != nil {
		return out, err
	}
	if detail.Run.Source != subagentSource || detail.Run.Channel != "subagent" {
		return out, notFound("The subagent review is unavailable in this project.")
	}
	var version int64
	err = tx.QueryRowContext(ctx, `SELECT m.version_revision FROM review_current_runs c JOIN review_run_memberships m ON m.project_id=c.project_id AND m.id=c.id AND m.start_revision=c.start_revision WHERE c.project_id=? AND c.id=?`, project, id).Scan(&version)
	if err != nil {
		return out, err
	}
	return SubagentReport{ID: id, ProjectID: project, PullNumber: detail.PullNumber, Revision: version, Run: detail.Run, Findings: detail.Findings}, nil
}
func readSubagentReportHTTP(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := subagentScope(w, r, store, "read")
		if !ok {
			return
		}
		var out SubagentReport
		err := store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			out, err = readSubagentReport(ctx, tx, scope.ProjectID, r.PathValue("run_id"))
			return err
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, out)
	}
}

func validateSubagentUpdate(in SubagentUpdateRequest) error {
	if in.ExpectedRevision == nil || *in.ExpectedRevision < 1 || *in.ExpectedRevision == math.MaxInt64 || len(in.Decisions) > 100 || len(in.Fixes) > 100 || len(in.Decisions)+len(in.Fixes) == 0 && !in.Finalize {
		return validationError("Use the current run revision and at least one bounded decision, fix or finalization.")
	}
	seen := map[int64]bool{}
	for _, d := range in.Decisions {
		if d.Finding < 0 || d.Finding >= 100 || seen[d.Finding] || !oneOf(d.Disposition, "accepted", "rejected") || !nativeAdjudicationText(d.Reason, 2048, true) {
			return validationError("Use unique finding indexes, decisions and explicit reasons.")
		}
		seen[d.Finding] = true
	}
	seen = map[int64]bool{}
	for _, f := range in.Fixes {
		if f.Finding < 0 || f.Finding >= 100 || seen[f.Finding] || f.ExpectedSHA == nil || *f.ExpectedSHA != "" && !fullSubagentSHA(*f.ExpectedSHA) || !fullSubagentSHA(f.FixSHA) || !nativeAdjudicationText(f.ProofNote, 2048, true) {
			return validationError("Use unique finding indexes, expected/full fix SHAs and proof.")
		}
		seen[f.Finding] = true
	}
	return nil
}
func updateSubagentReportHTTP(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := subagentScope(w, r, store, "write")
		if !ok {
			return
		}
		id := r.PathValue("run_id")
		if !subagentRunID.MatchString(id) {
			core.WriteError(w, validationError("Use a complete review_run_ identity."))
			return
		}
		var in SubagentUpdateRequest
		if err := subagentJSON(w, r, &in, []string{"expected_revision"}, map[string][]string{"decisions": {"finding", "disposition", "reason"}, "fixes": {"finding", "expected_sha", "fix_sha", "proof_note"}}); err != nil {
			core.WriteError(w, err)
			return
		}
		if err := validateSubagentUpdate(in); err != nil {
			core.WriteError(w, err)
			return
		}
		key, err := core.IdempotencyKey(r, "validation_failed")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		raw, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "reviews.subagent.update", Target: id, Key: key, Request: in}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			current, err := readSubagentReport(ctx, tx, scope.ProjectID, id)
			if err != nil {
				return nil, err
			}
			if current.Revision != *in.ExpectedRevision {
				return nil, &core.Error{Code: "revision_conflict", Message: "The subagent review changed; read its current run revision.", Status: 409, Details: map[string]int64{"current_revision": current.Revision}}
			}
			at := time.Now().UTC().Format(time.RFC3339Nano)
			findings := make([]NativeFindingUpdate, len(current.Findings))
			for i, f := range current.Findings {
				findings[i] = NativeFindingUpdate{SourceFindingID: f.SourceFindingID, Disposition: f.Disposition, DecisionReason: f.DecisionReason, Actor: f.Actor, DecidedAt: f.DecidedAt, FixSHA: f.FixSHA, ProofNote: f.ProofNote, ResolvedAt: f.ResolvedAt}
			}
			for _, d := range in.Decisions {
				if d.Finding >= int64(len(findings)) {
					return nil, validationError("The finding index is outside this review.")
				}
				f := &findings[d.Finding]
				if f.FixSHA != "" && d.Disposition != "accepted" {
					return nil, nativeAdjudicationConflict()
				}
				f.Disposition, f.DecisionReason, f.Actor, f.DecidedAt = d.Disposition, d.Reason, scope.PrincipalID, at
			}
			for _, fix := range in.Fixes {
				if fix.Finding >= int64(len(findings)) {
					return nil, validationError("The finding index is outside this review.")
				}
				f := &findings[fix.Finding]
				if f.Disposition != "accepted" || f.FixSHA != *fix.ExpectedSHA {
					return nil, nativeAdjudicationConflict()
				}
				f.FixSHA, f.ProofNote, f.ResolvedAt = fix.FixSHA, fix.ProofNote, at
			}
			finalized := current.Run.Finalized || in.Finalize
			var counts *NativeCounts
			if current.Run.Finalized {
				counts = &NativeCounts{Found: current.Run.Found, Accepted: current.Run.Accepted, Routed: current.Run.Routed}
			} else if in.Finalize {
				counts = &NativeCounts{Found: int64(len(findings))}
				for _, f := range findings {
					if f.Disposition == "unadjudicated" {
						return nil, validationError("Decide every finding before finalizing.")
					}
					if f.Disposition == "accepted" {
						counts.Accepted++
					}
				}
			}
			_, _, err = updateSourceAdjudication(ctx, tx, scope, subagentSource, NativeAdjudication{SourceRunID: current.Run.SourceRunID, Finalized: finalized, FrozenCounts: counts, Findings: findings})
			if err != nil {
				return nil, err
			}
			updated, err := readSubagentReport(ctx, tx, scope.ProjectID, id)
			if err != nil {
				return nil, err
			}
			return SubagentReceipt{ID: id, State: "updated", Revision: updated.Revision}, nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, json.RawMessage(raw))
	}
}

// A history import may retain an exact existing live report, but cannot create
// this producer or replace its capture, adjudication or proof outside its API.
func guardSubagentImport(ctx context.Context, tx HistoryQuery, project, source string, pulls []PullInput) error {
	if source != subagentSource {
		return nil
	}
	for _, pull := range pulls {
		if len(pull.Attempts) != 0 {
			return transferNativeConflict()
		}
		for _, run := range pull.Runs {
			version, changed, err := retainedRunState(ctx, tx, project, source, pull.Number, run)
			if err != nil {
				return err
			}
			if version == 0 || changed || run.Channel != "subagent" {
				return transferNativeConflict()
			}
			summary := summarizeRun(project, source, pull.Number, run)
			if err := transferNativeExactRecord(ctx, tx, project, historyRunRecord, []any{summary.ID}, []any{pull.Number, source, run.SourceRunID, run.Channel, run.HeadSHA, run.Reviewer, run.Scope, run.Outcome, run.Attributable, run.Complete, run.StartedAt, run.FinishedAt, run.Found, run.Accepted, run.Routed, run.Finalized, summary.Fixed, mustJSON(run.CoverageLimits)}); err != nil {
				return err
			}
		}
	}
	return nil
}
