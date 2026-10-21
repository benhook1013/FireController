package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// ProgressSchemaSQL is the canonical schema-eight extension for backup inventory.
func ProgressSchemaSQL() []string {
	return []string{
		`CREATE TABLE job_progress(project_id TEXT NOT NULL,job_id TEXT NOT NULL,job_revision INTEGER NOT NULL CHECK(job_revision>=1),summary TEXT NOT NULL,next TEXT NOT NULL,blocker TEXT NOT NULL,evidence_json TEXT NOT NULL,PRIMARY KEY(project_id,job_id),FOREIGN KEY(project_id,job_id) REFERENCES jobs(project_id,id) ON DELETE CASCADE);`,
		`CREATE TABLE job_progress_history(sequence INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT NOT NULL UNIQUE,project_id TEXT NOT NULL,job_id TEXT NOT NULL,job_revision INTEGER NOT NULL CHECK(job_revision>=1),actor_id TEXT NOT NULL,worker_id TEXT NOT NULL,summary TEXT NOT NULL,next TEXT NOT NULL,blocker TEXT NOT NULL,evidence_json TEXT NOT NULL,created_at TEXT NOT NULL,FOREIGN KEY(project_id,job_id) REFERENCES jobs(project_id,id) ON DELETE CASCADE);`,
		`CREATE INDEX job_progress_history_project_job_sequence ON job_progress_history(project_id,job_id,sequence DESC);`,
	}
}

type progress struct {
	Summary  string   `json:"summary"`
	Next     string   `json:"next"`
	Blocker  string   `json:"blocker"`
	Evidence []string `json:"evidence"`
}
type progressPatch struct {
	Summary  *string   `json:"summary,omitempty"`
	Next     *string   `json:"next,omitempty"`
	Blocker  *string   `json:"blocker,omitempty"`
	Evidence *[]string `json:"evidence,omitempty"`
}

func rejectNullFields(data []byte, out any) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return strictjson.ErrSyntax
	}
	for _, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return strictjson.ErrSyntax
		}
	}
	return strictjson.Decode(data, out)
}
func (p *progressPatch) UnmarshalJSON(data []byte) error {
	type wire progressPatch
	var v wire
	if err := rejectNullFields(data, &v); err != nil {
		return err
	}
	*p = progressPatch(v)
	return nil
}
func (r *editRequest) UnmarshalJSON(data []byte) error {
	type wire editRequest
	var v wire
	if err := rejectNullFields(data, &v); err != nil {
		return err
	}
	*r = editRequest(v)
	return nil
}
func emptyProgress() progress { return progress{Evidence: []string{}} }
func validProgress(p progress) bool {
	if !validCheckpointText(p.Summary, 8<<10, false) || !validCheckpointText(p.Next, 8<<10, false) || !validCheckpointText(p.Blocker, 4<<10, false) || p.Evidence == nil || len(p.Evidence) > 10 {
		return false
	}
	size := len(p.Summary) + len(p.Next) + len(p.Blocker)
	for _, ref := range p.Evidence {
		if !validCheckpointText(ref, 1024, true) {
			return false
		}
		size += len(ref)
	}
	return size <= 16<<10
}
func progressUnavailable() error {
	return core.Fail("storage_unavailable", "Stored job progress is unavailable.", 500)
}
func decodeProgress(summary, next, blocker, evidence string) (progress, error) {
	p := progress{Summary: summary, Next: next, Blocker: blocker}
	if len(evidence) > 64<<10 || strictjson.Decode([]byte(evidence), &p.Evidence) != nil || !validProgress(p) {
		return p, progressUnavailable()
	}
	encoded, _ := json.Marshal(p.Evidence)
	if string(encoded) != evidence {
		return p, progressUnavailable()
	}
	return p, nil
}
func readProgress(ctx context.Context, q queryer, project, id string, revision int64) (progress, int64, error) {
	var summary, next, blocker, evidence sql.NullString
	var last int64
	err := q.QueryRowContext(ctx, `SELECT job_revision,CASE WHEN typeof(summary)='text' AND length(CAST(summary AS BLOB))<=8192 THEN summary END,CASE WHEN typeof(next)='text' AND length(CAST(next AS BLOB))<=8192 THEN next END,CASE WHEN typeof(blocker)='text' AND length(CAST(blocker AS BLOB))<=4096 THEN blocker END,CASE WHEN typeof(evidence_json)='text' AND length(CAST(evidence_json AS BLOB))<=65536 THEN evidence_json END FROM job_progress WHERE project_id=? AND job_id=?`, project, id).Scan(&last, &summary, &next, &blocker, &evidence)
	if errors.Is(err, sql.ErrNoRows) {
		return progress{}, 0, progressUnavailable()
	}
	if err != nil {
		return progress{}, 0, err
	}
	if last < 1 || last > revision || !summary.Valid || !next.Valid || !blocker.Valid || !evidence.Valid {
		return progress{}, 0, progressUnavailable()
	}
	p, err := decodeProgress(summary.String, next.String, blocker.String, evidence.String)
	return p, last, err
}

type progressWriter interface {
	queryer
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func seedEmptyProgress(ctx context.Context, q progressWriter, j job) error {
	_, err := q.ExecContext(ctx, `INSERT INTO job_progress(project_id,job_id,job_revision,summary,next,blocker,evidence_json) VALUES(?,?,?,'','','','[]')`, j.ProjectID, j.ID, j.Revision)
	return err
}
func storeProgress(ctx context.Context, q progressWriter, j job, p progress, id, actor, worker, at string) error {
	if !validProgress(p) {
		return progressUnavailable()
	}
	refs, _ := json.Marshal(p.Evidence)
	if _, err := q.ExecContext(ctx, `INSERT INTO job_progress_history(id,project_id,job_id,job_revision,actor_id,worker_id,summary,next,blocker,evidence_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, j.ProjectID, j.ID, j.Revision, actor, worker, p.Summary, p.Next, p.Blocker, string(refs), at); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, `UPDATE job_progress SET job_revision=?,summary=?,next=?,blocker=?,evidence_json=? WHERE project_id=? AND job_id=?`, j.Revision, p.Summary, p.Next, p.Blocker, string(refs), j.ProjectID, j.ID)
	return err
}

type progressEntry struct {
	ID          string   `json:"id"`
	ProjectID   string   `json:"project_id"`
	JobID       string   `json:"job_id"`
	JobRevision int64    `json:"job_revision"`
	Sequence    int64    `json:"sequence"`
	ActorID     string   `json:"actor_id"`
	WorkerID    string   `json:"worker_id"`
	Progress    progress `json:"progress"`
	CreatedAt   string   `json:"created_at"`
}
type progressPage struct {
	Items   []progressEntry `json:"items"`
	Cursor  string          `json:"cursor,omitempty"`
	HasMore bool            `json:"has_more"`
}

const progressHistoryColumns = `id,project_id,job_id,job_revision,sequence,actor_id,worker_id,CASE WHEN typeof(summary)='text' AND length(CAST(summary AS BLOB))<=8192 THEN summary END,CASE WHEN typeof(next)='text' AND length(CAST(next AS BLOB))<=8192 THEN next END,CASE WHEN typeof(blocker)='text' AND length(CAST(blocker AS BLOB))<=4096 THEN blocker END,CASE WHEN typeof(evidence_json)='text' AND length(CAST(evidence_json AS BLOB))<=65536 THEN evidence_json END,created_at`

func scanProgressEntry(row checkpointScanner, jobRevision int64) (progressEntry, error) {
	var v progressEntry
	var summary, next, blocker, refs sql.NullString
	err := row.Scan(&v.ID, &v.ProjectID, &v.JobID, &v.JobRevision, &v.Sequence, &v.ActorID, &v.WorkerID, &summary, &next, &blocker, &refs, &v.CreatedAt)
	if err != nil {
		return v, err
	}
	if !validPrimaryID(v.ID) || !validPrimaryID(v.ProjectID) || !validPrimaryID(v.JobID) || !validPrimaryID(v.ActorID) || v.WorkerID != "" && !validPrimaryID(v.WorkerID) || v.JobRevision < 1 || v.JobRevision > jobRevision || v.Sequence < 1 || !contextTime(v.CreatedAt) || !summary.Valid || !next.Valid || !blocker.Valid || !refs.Valid {
		return v, progressUnavailable()
	}
	v.Progress, err = decodeProgress(summary.String, next.String, blocker.String, refs.String)
	return v, err
}
func currentProgressHistory(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err = core.QueryKeys(r, "invalid_query", "limit", "cursor"); err != nil {
			core.WriteError(w, err)
			return
		}
		limit, err := core.PageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		id := r.PathValue("job_id")
		filter := "job-progress:" + scope.ProjectID + ":" + id + ":sequence-desc"
		before, err := progressCursor(store, scope, filter, r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result := progressPage{Items: []progressEntry{}}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			j, err := readJob(ctx, tx, scope.ProjectID, id)
			if err != nil {
				return err
			}
			query := `SELECT ` + progressHistoryColumns + ` FROM job_progress_history WHERE project_id=? AND job_id=?`
			args := []any{scope.ProjectID, id}
			if before > 0 {
				query += ` AND sequence<?`
				args = append(args, before)
			}
			query += ` ORDER BY sequence DESC LIMIT ?`
			args = append(args, limit+1)
			rows, err := tx.QueryContext(ctx, query, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				v, err := scanProgressEntry(rows, j.Revision)
				if err != nil {
					return err
				}
				if len(result.Items) == limit {
					result.HasMore = true
					break
				}
				result.Items = append(result.Items, v)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if result.HasMore {
				result.Cursor = store.Cursor(scope, filter, strconv.FormatInt(result.Items[len(result.Items)-1].Sequence, 10))
			}
			return nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		writeProgressJSON(w, result, maxProgressHistory)
	}
}

// ValidateProgressSnapshot verifies canonical current records and the retained
// progress archive without rewriting either or consulting provider state.
func ValidateProgressSnapshot(ctx context.Context, tx *sql.Tx) error {
	// Reject malformed keys before keyset traversal, including an empty pair
	// that would otherwise sort at or before the initial cursor.
	var badJob bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE typeof(project_id)!='text' OR project_id='' OR typeof(id)!='text' OR id='' OR typeof(revision)!='integer' OR revision<1)`).Scan(&badJob); err != nil {
		return err
	}
	if badJob {
		return progressUnavailable()
	}
	for _, table := range []string{"job_progress", "job_progress_history"} {
		invalidTypes := `typeof(project_id)!='text' OR typeof(job_id)!='text' OR typeof(job_revision)!='integer' OR typeof(summary)!='text' OR typeof(next)!='text' OR typeof(blocker)!='text' OR typeof(evidence_json)!='text'`
		if table == "job_progress_history" {
			invalidTypes += ` OR typeof(sequence)!='integer' OR typeof(id)!='text' OR typeof(actor_id)!='text' OR typeof(worker_id)!='text' OR typeof(created_at)!='text'`
		}
		var bad bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE `+invalidTypes+`)`).Scan(&bad); err != nil {
			return err
		}
		if bad {
			return progressUnavailable()
		}
	}
	type owner struct {
		project, id string
		revision    int64
	}
	project, id := "", ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT project_id,id,revision FROM jobs WHERE (project_id,id)>(?,?) ORDER BY project_id,id LIMIT 64`, project, id)
		if err != nil {
			return err
		}
		var owners []owner
		for rows.Next() {
			var v owner
			if err = rows.Scan(&v.project, &v.id, &v.revision); err != nil {
				break
			}
			owners = append(owners, v)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}

		for _, j := range owners {
			if !validPrimaryID(j.project) || !validPrimaryID(j.id) || j.revision < 1 {
				return progressUnavailable()
			}
			current, revision, err := readProgress(ctx, tx, j.project, j.id, j.revision)
			if err != nil {
				return err
			}
			history, err := tx.QueryContext(ctx, `SELECT `+progressHistoryColumns+` FROM job_progress_history WHERE project_id=? AND job_id=? ORDER BY sequence DESC`, j.project, j.id)
			if err != nil {
				return err
			}
			first := true
			for history.Next() {
				v, e := scanProgressEntry(history, j.revision)
				if e != nil {
					history.Close()
					return e
				}
				if first && (!reflect.DeepEqual(v.Progress, current) || v.JobRevision != revision) {
					history.Close()
					return progressUnavailable()
				}
				first = false
			}
			err = history.Err()
			history.Close()
			if err != nil {
				return err
			}
			if first && !reflect.DeepEqual(current, emptyProgress()) {
				return progressUnavailable()
			}
		}
		if len(owners) < 64 {
			break
		}
		last := owners[len(owners)-1]
		project, id = last.project, last.id
	}

	for _, table := range []string{"job_progress", "job_progress_history"} {
		rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check("`+table+`")`)
		if err != nil {
			return err
		}
		bad := rows.Next()
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if bad {
			return progressUnavailable()
		}
	}
	return nil
}

func applyProgressPatch(p progress, patch *progressPatch) progress {
	if patch == nil {
		return p
	}
	if patch.Summary != nil {
		p.Summary = *patch.Summary
	}
	if patch.Next != nil {
		p.Next = *patch.Next
	}
	if patch.Blocker != nil {
		p.Blocker = *patch.Blocker
	}
	if patch.Evidence != nil {
		p.Evidence = append([]string{}, (*patch.Evidence)...)
	}
	return p
}

type remainingChecklist struct {
	Revision  int64           `json:"revision"`
	Remaining []checklistItem `json:"remaining"`
	Completed int             `json:"completed"`
	Total     int             `json:"total"`
}
type currentJob struct {
	job
	Progress  progress           `json:"progress"`
	Checklist remainingChecklist `json:"checklist"`
	Notes     notePage           `json:"notes"`
}

func readCurrentJob(ctx context.Context, tx *sql.Tx, store *core.Store, scope core.Scope, id string) (currentJob, error) {
	var result currentJob
	var err error
	result.job, err = readJob(ctx, tx, scope.ProjectID, id)
	if err != nil {
		return result, err
	}
	if !validContextJob(result.job) {
		return result, contextUnavailable()
	}
	result.Progress, _, err = readProgress(ctx, tx, scope.ProjectID, id, result.Revision)
	if err != nil {
		return result, err
	}
	revision, items, err := readChecklist(ctx, tx, scope.ProjectID, id)
	if err != nil {
		return result, err
	}
	result.Checklist = remainingChecklist{Revision: revision, Remaining: []checklistItem{}, Total: len(items)}
	for _, item := range items {
		if item.Done {
			result.Checklist.Completed++
		} else {
			result.Checklist.Remaining = append(result.Checklist.Remaining, item)
		}
	}
	result.Notes, err = readNotePage(ctx, tx, store, scope, noteListFilter{Route: "notes-current", JobID: id, Status: "pending"}, 5, nil)
	return result, err
}
