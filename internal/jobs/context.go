package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
)

const maxJobContext = 1 << 20

type jobContext struct {
	ProjectID   string            `json:"project_id"`
	WorkerID    string            `json:"worker_id"`
	Job         job               `json:"job"`
	Checkpoint  *fullCheckpoint   `json:"checkpoint"`
	Checklist   checklistResponse `json:"checklist"`
	Primary     primaryJob        `json:"primary"`
	Pause       workerPause       `json:"pause"`
	Notes       notePage          `json:"notes"`
	Inbox       inbox.PendingPage `json:"inbox"`
	EventCursor string            `json:"event_cursor"`
	Complete    bool              `json:"complete"`
	Omitted     []string          `json:"omitted"`
}

func contextUnavailable() error {
	return core.Fail("storage_unavailable", "Stored job context is unavailable.", 500)
}
func contextTime(value string) bool {
	at, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !at.IsZero() && len(value) <= 64
}
func validContextJob(value job) bool {
	return validPrimaryID(value.ID) && validPrimaryID(value.ProjectID) && (value.WorkerID == "" || validPrimaryID(value.WorkerID)) && value.Revision > 0 && value.Title == strings.TrimSpace(value.Title) && utf8.ValidString(value.Title) && utf8.ValidString(value.Brief) && validateContent(value.Title, value.Brief, value.Status) == nil && contextTime(value.CreatedAt) && contextTime(value.UpdatedAt)
}
func validContextCheckpoint(value fullCheckpoint, current job) bool {
	return value.ProjectID == current.ProjectID && value.JobID == current.ID && validPrimaryID(value.ID) && validPrimaryID(value.ActorID) && (value.WorkerID == "" || validPrimaryID(value.WorkerID)) && value.Sequence > 0 && value.JobRevision <= current.Revision && contextTime(value.CreatedAt) && value.Evidence != nil && validateCheckpoint(checkpointRequest{ExpectedJobRevision: value.JobRevision, Done: value.Done, Next: value.Next, Blocker: &value.Blocker, Evidence: value.Evidence}) == nil
}

func getJobContext(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if scope.WorkerID == "" {
			core.WriteError(w, core.Fail("worker_required", "Bind a worker before reading job context.", 400))
			return
		}
		if _, err := core.QueryKeys(r, "invalid_query", "limit"); err != nil {
			core.WriteError(w, err)
			return
		}
		limit, err := core.PageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		id := r.PathValue("job_id")
		if !validPrimaryID(id) {
			core.WriteError(w, core.Fail("validation_failed", "Select a valid job identity.", 400))
			return
		}
		var result jobContext
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			result, err = readJobContext(ctx, tx, store, scope, id, limit)
			return err
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		data, err := json.Marshal(result)
		if err != nil {
			core.WriteError(w, contextUnavailable())
			return
		}
		if len(data)+1 > maxJobContext {
			core.WriteError(w, core.Fail("context_too_large", "Read the job, latest checkpoint, checklist, worker, notes and inbox separately; job context exceeds its encoded limit.", 413))
			return
		}
		core.WriteJSON(w, 200, json.RawMessage(data))
	}
}

func readJobContext(ctx context.Context, tx *sql.Tx, store *core.Store, scope core.Scope, id string, limit int) (jobContext, error) {
	result := jobContext{ProjectID: scope.ProjectID, WorkerID: scope.WorkerID, Omitted: []string{}}
	var err error
	result.Job, err = readJob(ctx, tx, scope.ProjectID, id)
	if err != nil {
		return result, err
	}
	if !validContextJob(result.Job) {
		return result, contextUnavailable()
	}
	checkpoint, err := scanLatestCheckpoint(ctx, tx, scope.ProjectID, id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if err == nil {
		if !validContextCheckpoint(checkpoint, result.Job) {
			return result, contextUnavailable()
		}
		result.Checkpoint = &checkpoint
	}
	result.Checklist = checklistResponse{JobID: id, JobRevision: result.Job.Revision}
	result.Checklist.Revision, result.Checklist.Items, err = readChecklist(ctx, tx, scope.ProjectID, id)
	if err != nil {
		return result, err
	}
	result.Primary, err = readPrimaryJob(ctx, tx, scope.ProjectID, scope.WorkerID)
	if err != nil {
		return result, err
	}
	result.Pause, err = readWorkerPause(ctx, tx, scope.ProjectID, scope.WorkerID)
	if err != nil {
		return result, err
	}
	filter := noteListFilter{Route: "notes-current", WorkerID: scope.WorkerID, Status: "pending", IncludeAssignedJobs: true}
	result.Notes, err = readNotePage(ctx, tx, store, scope, filter, limit, nil)
	if err != nil {
		return result, err
	}
	result.Inbox, result.EventCursor, err = inbox.ReadPendingContext(ctx, tx, store, scope, limit)
	if err != nil {
		return result, err
	}
	if len(result.Notes.Items) > 0 {
		result.Omitted = append(result.Omitted, "note_bodies")
	}
	if result.Notes.HasMore {
		result.Omitted = append(result.Omitted, "additional_notes")
	}
	if len(result.Inbox.Items) > 0 {
		result.Omitted = append(result.Omitted, "message_bodies")
	}
	if result.Inbox.HasMore {
		result.Omitted = append(result.Omitted, "additional_messages")
	}
	result.Complete = len(result.Omitted) == 0
	return result, nil
}
