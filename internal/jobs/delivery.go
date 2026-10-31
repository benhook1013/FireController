package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/presentation"
)

const (
	defaultDeliveryWorkerLimit = 20
	defaultDeliveryJobLimit    = 50
	maxDeliveryPreviewBytes    = 240
)

type deliveryWorker struct {
	ID                   string `json:"id"`
	ProjectID            string `json:"project_id"`
	Name                 string `json:"name"`
	PrimaryJobID         string `json:"primary_job_id"`
	Paused               bool   `json:"paused"`
	PauseReason          string `json:"pause_reason"`
	PauseReasonTruncated bool   `json:"pause_reason_truncated"`
}

type deliveryProgress struct {
	Summary          string `json:"summary"`
	Next             string `json:"next"`
	Blocker          string `json:"blocker"`
	SummaryTruncated bool   `json:"summary_truncated"`
	NextTruncated    bool   `json:"next_truncated"`
	BlockerTruncated bool   `json:"blocker_truncated"`
}

type deliveryChecklist struct {
	Revision    int64  `json:"revision"`
	JobRevision int64  `json:"job_revision"`
	Total       int    `json:"total"`
	Done        int    `json:"done"`
	CreatedAt   string `json:"created_at"`
}

type deliveryJob struct {
	Summary               *Summary           `json:"summary,omitempty"`
	ID                    string             `json:"id"`
	Title                 string             `json:"title"`
	WorkerID              string             `json:"worker_id"`
	WorkerName            string             `json:"worker_name"`
	Status                string             `json:"status"`
	Revision              int64              `json:"revision"`
	BriefPreview          string             `json:"brief_preview"`
	BriefPreviewTruncated bool               `json:"brief_preview_truncated"`
	UpdatedAt             string             `json:"updated_at"`
	Progress              deliveryProgress   `json:"progress"`
	Checklist             *deliveryChecklist `json:"checklist"`
}

type deliveryPage[T any] struct {
	Items   []T    `json:"items"`
	Cursor  string `json:"cursor,omitempty"`
	HasMore bool   `json:"has_more"`
}

type deliveryResponse struct {
	Workers deliveryPage[deliveryWorker] `json:"workers"`
	Jobs    deliveryPage[deliveryJob]    `json:"jobs"`
}

func deliveryLimit(r *http.Request, key string, fallback int) (int, error) {
	values, present := r.URL.Query()[key]
	if !present {
		return fallback, nil
	}
	if len(values) != 1 {
		return 0, core.Fail("invalid_query", "Use one valid page limit for each delivery collection.", http.StatusBadRequest)
	}
	limit, err := strconv.Atoi(values[0])
	if err != nil || limit < 1 || limit > 100 {
		return 0, core.Fail("invalid_query", "Use one delivery page limit between 1 and 100.", http.StatusBadRequest)
	}
	return limit, nil
}

func delivery(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		query, err := core.QueryKeys(r, "invalid_query", "worker_limit", "job_limit", "worker_cursor", "job_cursor")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		workerLimit, err := deliveryLimit(r, "worker_limit", defaultDeliveryWorkerLimit)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		jobLimit, err := deliveryLimit(r, "job_limit", defaultDeliveryJobLimit)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		workerAfter, err := store.ParseCursor(scope, presentation.WorkerOrderCursorKind("delivery-workers"), query.Get("worker_cursor"))
		if err != nil || workerAfter != "" && !validPrimaryID(workerAfter) {
			core.WriteError(w, core.Fail("invalid_cursor", "Use a valid cursor from this delivery worker page.", http.StatusBadRequest))
			return
		}
		jobAfterRaw, err := store.ParseCursor(scope, "delivery-open-jobs:status-id:v1", query.Get("job_cursor"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
		afterStatus, afterID, err := parseDeliveryJobCursor(jobAfterRaw)
		if err != nil {
			core.WriteError(w, err)
			return
		}

		result := deliveryResponse{
			Workers: deliveryPage[deliveryWorker]{Items: make([]deliveryWorker, 0, workerLimit)},
			Jobs:    deliveryPage[deliveryJob]{Items: make([]deliveryJob, 0, jobLimit)},
		}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			if err := readDeliveryWorkers(ctx, tx, scope.ProjectID, workerAfter, workerLimit, scope, store, &result.Workers); err != nil {
				return err
			}
			return readDeliveryJobs(ctx, tx, scope.ProjectID, afterStatus, afterID, jobLimit, scope, store, &result.Jobs)
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, result)
	}
}

func parseDeliveryJobCursor(raw string) (string, string, error) {
	if raw == "" {
		return "", "", nil
	}
	var position []string
	if json.Unmarshal([]byte(raw), &position) != nil || len(position) != 2 ||
		(position[0] != "active" && position[0] != "blocked" && position[0] != "parked") || !validPrimaryID(position[1]) {
		return "", "", core.Fail("invalid_cursor", "Use a valid cursor from this delivery open-job page.", http.StatusBadRequest)
	}
	return position[0], position[1], nil
}

func readDeliveryWorkers(ctx context.Context, tx *sql.Tx, project, after string, limit int, scope core.Scope, store *core.Store, page *deliveryPage[deliveryWorker]) error {
	order, err := presentation.WorkerOrderJSON(ctx, tx, project)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, presentation.WorkerOrderCTE+`SELECT w.id,w.project_id,w.name,COALESCE(p.job_id,''),COALESCE(s.paused,0),COALESCE(s.reason,'')
		FROM ordered_workers w
		LEFT JOIN worker_primary_jobs p ON p.project_id=w.project_id AND p.worker_id=w.id
		LEFT JOIN worker_pause_state s ON s.project_id=w.project_id AND s.worker_id=w.id`+presentation.WorkerOrderTail, order, project, after, after, limit+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item deliveryWorker
		var paused int
		if err := rows.Scan(&item.ID, &item.ProjectID, &item.Name, &item.PrimaryJobID, &paused, &item.PauseReason); err != nil {
			return err
		}
		if len(page.Items) == limit {
			page.HasMore = true
			break
		}
		// Worker names use their admission bounds, not checkpoint control rules.
		validName := item.Name != "" && item.Name == strings.TrimSpace(item.Name) && len(item.Name) <= 200 && utf8.ValidString(item.Name)
		if item.ProjectID != project || !validPrimaryID(item.ID) || !validName || item.PrimaryJobID != "" && !validPrimaryID(item.PrimaryJobID) || paused != 0 && paused != 1 || !validPauseReason(paused == 1, item.PauseReason) {
			return core.Fail("storage_unavailable", "Stored delivery worker state is unavailable.", http.StatusInternalServerError)
		}
		item.Paused = paused == 1
		item.PauseReason, item.PauseReasonTruncated = deliveryPreview(item.PauseReason)
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if page.HasMore {
		page.Cursor = store.Cursor(scope, presentation.WorkerOrderCursorKind("delivery-workers"), page.Items[len(page.Items)-1].ID)
	}
	return nil
}

func readDeliveryJobs(ctx context.Context, tx *sql.Tx, project, afterStatus, afterID string, limit int, scope core.Scope, store *core.Store, page *deliveryPage[deliveryJob]) error {
	statuses := []string{"active", "blocked", "parked"}
	branches := make([]string, 0, len(statuses))
	args := make([]any, 0, len(statuses)*4+2)
	for _, status := range statuses {
		if afterStatus != "" && status < afterStatus {
			continue
		}
		branch := `SELECT id,status FROM jobs WHERE project_id=? AND status=?`
		args = append(args, project, status)
		if status == afterStatus {
			branch += ` AND id>?`
			args = append(args, afterID)
		}
		branch += ` ORDER BY id LIMIT ?`
		args = append(args, limit+1)
		branches = append(branches, `SELECT * FROM (`+branch+`)`)
	}
	if len(branches) == 0 {
		return nil
	}
	query := `WITH candidates AS (` + strings.Join(branches, ` UNION ALL `) + `),
		selected AS (SELECT id,status FROM candidates ORDER BY status,id LIMIT ?)
		SELECT j.id,j.title,COALESCE(j.worker_id,''),COALESCE(w.name,''),j.status,j.revision,j.brief,j.updated_at,
		p.job_revision,CASE WHEN typeof(p.summary)='text' AND length(CAST(p.summary AS BLOB))<=8192 THEN substr(p.summary,1,244) END,CASE WHEN typeof(p.next)='text' AND length(CAST(p.next AS BLOB))<=8192 THEN substr(p.next,1,244) END,CASE WHEN typeof(p.blocker)='text' AND length(CAST(p.blocker AS BLOB))<=4096 THEN substr(p.blocker,1,244) END,
		l.revision,l.job_revision,l.items_json,l.created_at,
		s.revision,s.title,s.body
		FROM selected x JOIN jobs j ON j.project_id=? AND j.id=x.id
		LEFT JOIN workers w ON w.project_id=j.project_id AND w.id=j.worker_id
        LEFT JOIN job_progress p ON p.project_id=j.project_id AND p.job_id=j.id
		LEFT JOIN job_checklists l ON l.project_id=j.project_id AND l.job_id=j.id
		LEFT JOIN job_summaries s ON s.project_id=j.project_id AND s.job_id=j.id
		ORDER BY x.status,x.id`
	args = append(args, limit+1, project)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	lastID, lastStatus := "", ""
	for rows.Next() {
		var item deliveryJob
		var progressRevision sql.NullInt64
		var summary, next, blocker sql.NullString
		var brief string
		var checklistRevision, checklistJobRevision sql.NullInt64
		var checklistRaw, checklistAt sql.NullString
		var summaryRevision sql.NullInt64
		var summaryTitle, summaryBody sql.NullString
		if err := rows.Scan(&item.ID, &item.Title, &item.WorkerID, &item.WorkerName, &item.Status, &item.Revision, &brief, &item.UpdatedAt, &progressRevision, &summary, &next, &blocker, &checklistRevision, &checklistJobRevision, &checklistRaw, &checklistAt, &summaryRevision, &summaryTitle, &summaryBody); err != nil {
			return err
		}
		if len(page.Items) == limit {
			page.HasMore = true
			break
		}
		// Keep titles and joined names exact under their existing write contracts.
		validTitle := item.Title != "" && item.Title == strings.TrimSpace(item.Title) && len(item.Title) <= maxTitle && utf8.ValidString(item.Title)
		validWorkerName := item.WorkerName == "" || item.WorkerName == strings.TrimSpace(item.WorkerName) && len(item.WorkerName) <= 200 && utf8.ValidString(item.WorkerName)
		if !validPrimaryID(item.ID) || !validTitle || !validWorkerName || item.WorkerID != "" && !validPrimaryID(item.WorkerID) || !validStatuses[item.Status] || item.Status == "completed" || item.Revision < 1 || item.WorkerID == "" && item.WorkerName != "" || item.WorkerID != "" && item.WorkerName == "" || len(brief) > maxBrief || !utf8.ValidString(brief) || !contextTime(item.UpdatedAt) {
			return core.Fail("storage_unavailable", "Stored delivery job state is unavailable.", http.StatusInternalServerError)
		}
		item.BriefPreview, item.BriefPreviewTruncated = deliveryPreview(brief)
		if !progressRevision.Valid || progressRevision.Int64 < 1 || progressRevision.Int64 > item.Revision || !summary.Valid || !next.Valid || !blocker.Valid || !utf8.ValidString(summary.String) || !utf8.ValidString(next.String) || !utf8.ValidString(blocker.String) {
			return progressUnavailable()
		}
		item.Progress.Summary, item.Progress.SummaryTruncated = deliveryPreview(summary.String)
		item.Progress.Next, item.Progress.NextTruncated = deliveryPreview(next.String)
		item.Progress.Blocker, item.Progress.BlockerTruncated = deliveryPreview(blocker.String)
		if checklistRevision.Valid || checklistJobRevision.Valid || checklistRaw.Valid || checklistAt.Valid {
			if !checklistRevision.Valid || !checklistJobRevision.Valid || !checklistRaw.Valid || !checklistAt.Valid || checklistRevision.Int64 < 1 || checklistJobRevision.Int64 < 1 || checklistJobRevision.Int64 > item.Revision || !contextTime(checklistAt.String) {
				return core.Fail("storage_unavailable", "Stored delivery checklist summary is unavailable.", http.StatusInternalServerError)
			}
			items, err := decodeChecklistItems(checklistRaw.String)
			if err != nil {
				return err
			}
			item.Checklist = &deliveryChecklist{Revision: checklistRevision.Int64, JobRevision: checklistJobRevision.Int64, Total: len(items), CreatedAt: checklistAt.String}
			for _, entry := range items {
				if entry.Done {
					item.Checklist.Done++
				}
			}
		}
		if summaryRevision.Valid || summaryTitle.Valid || summaryBody.Valid {
			if !summaryRevision.Valid || summaryRevision.Int64 < 1 || !summaryTitle.Valid || !summaryBody.Valid || !validSummary(summaryTitle.String, summaryBody.String) {
				return summaryUnavailable()
			}
			if summaryTitle.String != "" {
				item.Summary = &Summary{JobID: item.ID, JobRevision: item.Revision, Revision: summaryRevision.Int64, Title: summaryTitle.String, Body: summaryBody.String}
			}
		}
		lastID, lastStatus = item.ID, item.Status
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if page.HasMore && lastID != "" {
		position, _ := json.Marshal([]string{lastStatus, lastID})
		page.Cursor = store.Cursor(scope, "delivery-open-jobs:status-id:v1", string(position))
	}
	return nil
}

func deliveryPreview(value string) (string, bool) {
	if len(value) <= maxDeliveryPreviewBytes {
		return value, false
	}
	end := maxDeliveryPreviewBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end], true
}
