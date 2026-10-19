package reviews

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/publicsite"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// QueueEditRequest changes only selected queue metadata and visibility. It is
// not a provider observation and cannot publish runs or attempts.
type QueueEditRequest struct {
	ExpectedRevision *int64           `json:"expected_revision"`
	RepositoryID     int64            `json:"repository_id"`
	Source           string           `json:"source"`
	Queue            []QueuePullInput `json:"queue"`
	Retired          []RetiredPull    `json:"retired"`
}

type QueuePullInput struct {
	Diff         *PullDiff `json:"diff,omitempty"`
	IsDraft      *bool     `json:"is_draft,omitempty"`
	MergedAt     string    `json:"merged_at,omitempty"`
	Number       int64     `json:"number"`
	Title        string    `json:"title"`
	State        string    `json:"state"`
	HeadSHA      string    `json:"head_sha"`
	BaseRef      string    `json:"base_ref"`
	BaseSHA      string    `json:"base_sha"`
	CIState      string    `json:"ci_state"`
	Mergeability string    `json:"mergeability"`
}

func readQueueEditJSON(w http.ResponseWriter, r *http.Request, dst *QueueEditRequest) error {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxObservationBytes))
	if err != nil || strictjson.Decode(raw, dst) != nil {
		return validationError("Use one strict JSON object with declared fields, unique keys and valid Unicode.")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return validationError("Use one strict JSON object with declared fields, unique keys and valid Unicode.")
	}
	for _, field := range []string{"expected_revision", "repository_id", "source"} {
		if _, ok := fields[field]; !ok || string(fields[field]) == "null" {
			return validationError("Required queue edit fields must be present and non-null.")
		}
	}
	for _, field := range []string{"queue", "retired"} {
		value, ok := fields[field]
		if !ok {
			continue
		}
		if string(value) == "null" {
			return validationError("Queue and retirement arrays may be omitted or arrays, but not null.")
		}
		if field != "queue" {
			continue
		}
		var entries []map[string]json.RawMessage
		if err := json.Unmarshal(value, &entries); err != nil {
			return validationError("Queue and retirement arrays may be omitted or arrays, but not null.")
		}
		for _, entry := range entries {
			for _, required := range []string{"number", "title", "state", "ci_state", "mergeability"} {
				value, ok := entry[required]
				if !ok || string(value) == "null" {
					return validationError("Required queue metadata must be present and non-null.")
				}
			}
			for _, optional := range []string{"head_sha", "base_ref", "base_sha"} {
				if value, ok := entry[optional]; ok && string(value) == "null" {
					return validationError("Optional queue metadata must be omitted or non-null.")
				}
			}
		}
	}
	return nil
}

func validateQueueEdit(req *QueueEditRequest) error {
	if req.ExpectedRevision == nil || *req.ExpectedRevision < 0 || req.RepositoryID <= 0 || !validText(req.Source, 128, true) {
		return validationError("Provide an expected revision, repository identity and bounded source label.")
	}
	if req.Queue == nil {
		req.Queue = []QueuePullInput{}
	}
	if req.Retired == nil {
		req.Retired = []RetiredPull{}
	}
	if len(req.Queue)+len(req.Retired) < 1 || len(req.Queue)+len(req.Retired) > maxQueuePulls {
		return validationError("Select between one and 100 queue entries and retirements.")
	}
	seen := make(map[int64]struct{}, len(req.Queue)+len(req.Retired))
	for _, pull := range req.Queue {
		if pull.Number < 1 || pull.Number > 2147483647 || !validText(pull.Title, 500, true) ||
			!oneOf(pull.State, "open", "merged", "closed", "unknown") || !validSHA(pull.HeadSHA) ||
			!validText(pull.BaseRef, 256, false) || !validSHA(pull.BaseSHA) ||
			!oneOf(pull.CIState, "unknown", "pending", "success", "failure") ||
			!oneOf(pull.Mergeability, "unknown", "mergeable", "conflicting") {
			return validationError("A pull request contains invalid identity or summary fields.")
		}
		if err := (PullDisplayMetadata{pull.Diff, pull.IsDraft, pull.MergedAt}).Validate(pull.State); err != nil {
			return err
		}
		if _, exists := seen[pull.Number]; exists {
			return validationError("Queue entries and retirements must be unique and disjoint.")
		}
		seen[pull.Number] = struct{}{}
	}
	for _, retired := range req.Retired {
		if retired.Number < 1 || retired.Number > 2147483647 || !oneOf(retired.Reason, "superseded", "defunct") {
			return validationError("Retired pull requests require a valid number and reason.")
		}
		if _, exists := seen[retired.Number]; exists {
			return validationError("Queue entries and retirements must be unique and disjoint.")
		}
		seen[retired.Number] = struct{}{}
	}
	return nil
}

func editQueue(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.AdminScope(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
			core.WriteError(w, err)
			return
		}
		var req QueueEditRequest
		if err = readQueueEditJSON(w, r, &req); err != nil {
			core.WriteError(w, err)
			return
		}
		if err = validateQueueEdit(&req); err != nil {
			core.WriteError(w, err)
			return
		}
		body, err := json.Marshal(req)
		if err != nil || len(body) > maxObservationBytes {
			core.WriteError(w, validationError("The normalized queue edit exceeds 256 KiB."))
			return
		}
		key, err := core.IdempotencyKey(r, "validation_failed")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "reviews.queue.edit", Target: "workspace", Key: key, Request: req}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			return publishQueueEdit(ctx, tx, scope, req, body)
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusCreated, json.RawMessage(result))
	}
}

func publishQueueEdit(ctx context.Context, tx *sql.Tx, scope core.Scope, req QueueEditRequest, body []byte) (Receipt, error) {
	var repository int64
	if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, scope.ProjectID).Scan(&repository); err != nil {
		return Receipt{}, err
	}
	if repository != req.RepositoryID {
		return Receipt{}, validationError("repository_id does not match the configured project repository.")
	}
	var revision int64
	var previousID, observed string
	err := tx.QueryRowContext(ctx, `SELECT w.revision,w.snapshot_id,s.observed_at FROM review_workspace w JOIN review_snapshots s ON s.project_id=w.project_id AND s.snapshot_id=w.snapshot_id WHERE w.project_id=?`, scope.ProjectID).Scan(&revision, &previousID, &observed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		revision, previousID, observed = 0, "", ""
	}
	if revision != *req.ExpectedRevision {
		return Receipt{}, &core.Error{Code: "revision_conflict", Message: "The review observation revision changed.", Status: http.StatusConflict, Details: map[string]int64{"current_revision": revision}}
	}
	if revision == math.MaxInt64 {
		return Receipt{}, core.Fail("revision_exhausted", "The review workspace revision cannot advance further.", http.StatusConflict)
	}
	next := revision + 1
	id := core.NewID("review_snapshot")
	recorded := time.Now().UTC().Format(time.RFC3339Nano)
	if observed == "" {
		observed = recorded
	}
	digest := sha256.Sum256(body)
	if _, err := tx.ExecContext(ctx, `INSERT INTO review_snapshots(project_id,snapshot_id,revision,repository_id,source,observed_at,recorded_at,payload_sha256) VALUES(?,?,?,?,?,?,?,?)`, scope.ProjectID, id, next, req.RepositoryID, req.Source, observed, recorded, hex.EncodeToString(digest[:])); err != nil {
		return Receipt{}, err
	}
	write := newHistorySnapshotWrite(scope.ProjectID, next)
	var nextPosition int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position),-1)+1 FROM review_current_pulls WHERE project_id=?`, scope.ProjectID).Scan(&nextPosition); err != nil {
		return Receipt{}, err
	}
	for _, selected := range req.Queue {
		old, position, err := retainedTransferPullBody(ctx, tx, scope.ProjectID, selected.Number)
		if errors.Is(err, sql.ErrNoRows) {
			old = []any{selected.Title, selected.State, selected.HeadSHA, selected.BaseRef, selected.BaseSHA, selected.CIState, selected.Mergeability, int64(0), int64(0), int64(0), "[]", "[]", "{}"}
			position = nextPosition
			nextPosition++
		} else if err != nil {
			return Receipt{}, err
		} else {
			// Metadata-only edits preserve the compact completeness bit unless the
			// head changes; changed heads cannot inherit full-coverage status.
			if old[2] != selected.HeadSHA {
				old[7] = int64(0)
			}
		}
		metadataJSON, metadataErr := inheritedPullMetadata(old, PullInput{HeadSHA: selected.HeadSHA, State: selected.State, Diff: selected.Diff, IsDraft: selected.IsDraft, MergedAt: selected.MergedAt})
		if metadataErr != nil {
			return Receipt{}, metadataErr
		}
		old[12] = metadataJSON
		metadata := []any{selected.Title, selected.State, selected.HeadSHA, selected.BaseRef, selected.BaseSHA, selected.CIState, selected.Mergeability}
		copy(old[:7], metadata)
		if err := write.pull(ctx, tx, selected.Number, position, old); err != nil {
			return Receipt{}, err
		}
		if err := write.remove(ctx, tx, historyRetiredRecord, []any{selected.Number}); err != nil {
			return Receipt{}, err
		}
	}
	var nextRetired int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position),-1)+1 FROM review_current_retired WHERE project_id=?`, scope.ProjectID).Scan(&nextRetired); err != nil {
		return Receipt{}, err
	}
	for _, selected := range req.Retired {
		var exists, alreadyRetired bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_current_pulls WHERE project_id=? AND number=?)`, scope.ProjectID, selected.Number).Scan(&exists); err != nil {
			return Receipt{}, err
		}
		if !exists {
			return Receipt{}, validationError("A retirement must target a currently visible pull request.")
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_current_retired WHERE project_id=? AND number=?)`, scope.ProjectID, selected.Number).Scan(&alreadyRetired); err != nil {
			return Receipt{}, err
		}
		if alreadyRetired {
			return Receipt{}, validationError("A retirement must target a currently visible pull request.")
		}
		if err := write.retired(ctx, tx, selected.Number, nextRetired, selected.Reason); err != nil {
			return Receipt{}, err
		}
		nextRetired++
	}
	if err := write.applyCurrent(ctx, tx, id, revision); err != nil {
		return Receipt{}, err
	}
	if err := core.AppendEvent(ctx, tx, scope, "review.observed", id, next, map[string]any{"snapshot_id": id, "source": req.Source, "queue_edited": true}); err != nil {
		return Receipt{}, err
	}
	if err := publicsite.Refresh(ctx, tx, scope.ProjectID, PublicSiteReviewFacts); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: id, Revision: next, Outcome: "queue_edited"}, nil
}
