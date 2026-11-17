package projectmap

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/benhook1013/FireController/internal/core"
)

type jobRequest struct {
	ExpectedRevision *int64  `json:"expected_revision"`
	WorkstreamID     *string `json:"workstream_id"`
}
type jobReplacement struct {
	ExpectedRevision int64  `json:"expected_revision"`
	WorkstreamID     string `json:"workstream_id"`
}

func jobRevision(ctx context.Context, tx *sql.Tx, project, id string) (int64, error) {
	var rev int64
	err := tx.QueryRowContext(ctx, `SELECT revision FROM jobs WHERE project_id=? AND id=?`, project, id).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, notFound()
	}
	if err != nil {
		return 0, err
	}
	if rev < 1 {
		return 0, unavailable()
	}
	return rev, nil
}
func selectedJob(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("job_id")
	if !validID(id) {
		core.WriteError(w, invalid())
		return "", false
	}
	return id, true
}
func validAssociation(id, title string) bool {
	return id == "" && title == "" || entryID.MatchString(id) && named(title, 240)
}
func readAssociation(ctx context.Context, tx *sql.Tx, project, job string) (JobWorkstream, error) {
	value := JobWorkstream{JobID: job}
	err := tx.QueryRowContext(ctx, `SELECT revision,COALESCE(workstream_id,''),workstream_title FROM job_workstreams WHERE project_id=? AND job_id=?`, project, job).Scan(&value.Revision, &value.WorkstreamID, &value.WorkstreamTitle)
	if errors.Is(err, sql.ErrNoRows) {
		return value, nil
	}
	if err != nil {
		return JobWorkstream{}, err
	}
	if value.Revision < 1 || !validAssociation(value.WorkstreamID, value.WorkstreamTitle) {
		return JobWorkstream{}, unavailable()
	}
	if value.WorkstreamID != "" {
		var kind string
		if err := tx.QueryRowContext(ctx, `SELECT kind FROM project_map_entries WHERE project_id=? AND id=?`, project, value.WorkstreamID).Scan(&kind); err != nil {
			return JobWorkstream{}, unavailable()
		}
		if kind != "workstream" {
			return JobWorkstream{}, unavailable()
		}
	}

	return value, nil
}
func getJobWorkstream(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "read")
		if !ok {
			return
		}
		id, ok := selectedJob(w, r)
		if !ok {
			return
		}
		var value JobWorkstream
		err := store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := jobRevision(ctx, tx, scope.ProjectID, id); err != nil {
				return err
			}
			var err error
			value, err = readAssociation(ctx, tx, scope.ProjectID, id)
			return err
		})
		respond(w, value, err, 200)
	}
}
func replaceJobWorkstream(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "write")
		if !ok {
			return
		}
		id, ok := selectedJob(w, r)
		if !ok {
			return
		}
		var raw jobRequest
		if err := core.ReadJSON(w, r, &raw); err != nil {
			core.WriteError(w, err)
			return
		}
		if !validGuard(raw.ExpectedRevision, 0) || raw.WorkstreamID == nil || *raw.WorkstreamID != "" && !entryID.MatchString(*raw.WorkstreamID) {
			core.WriteError(w, invalid())
			return
		}
		req := jobReplacement{*raw.ExpectedRevision, *raw.WorkstreamID}
		key, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "jobs.workstream.replace", Target: id, Key: key, Request: req}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			jobRev, err := jobRevision(ctx, tx, scope.ProjectID, id)
			if err != nil {
				return nil, err
			}
			current, err := readAssociation(ctx, tx, scope.ProjectID, id)
			if err != nil {
				return nil, err
			}
			if current.Revision != req.ExpectedRevision {
				return nil, &core.Error{Code: "job_workstream_revision_conflict", Message: "Reload the job workstream before replacing it.", Status: 409, Details: map[string]int64{"current_revision": current.Revision}}
			}
			if current.WorkstreamID == req.WorkstreamID {
				return JobReceipt{id, current.Revision, "unchanged"}, nil
			}
			title := ""
			if req.WorkstreamID != "" {
				e, err := readEntry(ctx, tx, scope.ProjectID, req.WorkstreamID)
				if err != nil {
					return nil, err
				}
				if e.Kind != "workstream" || e.Archived {
					return nil, invalid()
				}
				title = e.Title
			}
			rev := current.Revision + 1
			at := stamp()
			_, err = tx.ExecContext(ctx, `INSERT INTO job_workstreams(project_id,job_id,revision,workstream_id,workstream_title) VALUES(?,?,?,NULLIF(?,''),?) ON CONFLICT(project_id,job_id) DO UPDATE SET revision=excluded.revision,workstream_id=excluded.workstream_id,workstream_title=excluded.workstream_title`, scope.ProjectID, id, rev, req.WorkstreamID, title)
			if err != nil {
				return nil, err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO job_workstream_history(project_id,job_id,revision,workstream_id,workstream_title,actor_id,job_revision,created_at) VALUES(?,?,?,NULLIF(?,''),?,?,?,?)`, scope.ProjectID, id, rev, req.WorkstreamID, title, scope.PrincipalID, jobRev, at)
			if err != nil {
				return nil, err
			}
			if err = core.AppendEvent(ctx, tx, scope, "job.workstream_changed", id, rev, map[string]string{"job_id": id, "workstream_id": req.WorkstreamID}); err != nil {
				return nil, err
			}
			return JobReceipt{id, rev, "updated"}, nil
		})
		respond(w, json.RawMessage(result), err, 200)
	}
}
func jobWorkstreamHistory(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "read", "limit", "cursor")
		if !ok {
			return
		}
		id, ok := selectedJob(w, r)
		if !ok {
			return
		}
		limit, err := pageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		filter := "job.workstream-history:" + id + ":revision-desc"
		before, err := revisionCursor(store, scope, filter, r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result := Page[JobWorkstreamHistory]{Items: make([]JobWorkstreamHistory, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := jobRevision(ctx, tx, scope.ProjectID, id); err != nil {
				return err
			}
			query, args := associationHistoryQuery(scope.ProjectID, id, before, limit+1)
			rows, err := tx.QueryContext(ctx, query, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var item JobWorkstreamHistory
				if err = rows.Scan(&item.Revision, &item.WorkstreamID, &item.WorkstreamTitle, &item.JobRevision, &item.ActorID, &item.CreatedAt); err != nil {
					return err
				}
				if item.Revision < 1 || item.JobRevision < 1 || !validAssociation(item.WorkstreamID, item.WorkstreamTitle) || !validID(item.ActorID) || !validStamp(item.CreatedAt) {
					return unavailable()
				}
				if len(result.Items) == limit {
					result.HasMore = true
					break
				}
				result.Items = append(result.Items, item)
			}
			if err = rows.Err(); err != nil {
				return err
			}
			if result.HasMore {
				result.Cursor = store.Cursor(scope, filter, strconv.FormatInt(result.Items[len(result.Items)-1].Revision, 10))
			}
			return nil
		})
		respond(w, result, err, 200)
	}
}

func associationHistoryQuery(project, id string, before int64, limit int) (string, []any) {
	query := `SELECT revision,COALESCE(workstream_id,''),workstream_title,job_revision,actor_id,created_at FROM job_workstream_history WHERE project_id=? AND job_id=?`
	args := []any{project, id}
	if before > 0 {
		query += ` AND revision<?`
		args = append(args, before)
	}
	return query + ` ORDER BY revision DESC LIMIT ?`, append(args, limit)
}
