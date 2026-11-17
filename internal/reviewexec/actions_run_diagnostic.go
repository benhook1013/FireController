package reviewexec

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type ActionsRunDiagnosticRun struct {
	ID           int64  `json:"id"`
	CheckSuiteID int64  `json:"check_suite_id"`
	WorkflowID   int64  `json:"workflow_id"`
	RunNumber    int64  `json:"run_number"`
	RunAttempt   int64  `json:"run_attempt"`
	HeadSHA      string `json:"head_sha"`
	Event        string `json:"event"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}
type ActionsRunDiagnosticJob struct {
	ID          int64  `json:"id"`
	CheckRunID  int64  `json:"check_run_id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
}
type ActionsRunDiagnostic struct {
	ProjectID    string                    `json:"project_id"`
	Repository   string                    `json:"repository"`
	RepositoryID int64                     `json:"repository_id"`
	ObservedAt   string                    `json:"observed_at"`
	Run          ActionsRunDiagnosticRun   `json:"run"`
	Limit        int                       `json:"limit"`
	Total        int                       `json:"total"`
	Pending      int                       `json:"pending"`
	Running      int                       `json:"running"`
	Completed    int                       `json:"completed"`
	Omitted      int                       `json:"omitted"`
	Capacity     string                    `json:"capacity"`
	Completeness string                    `json:"completeness"`
	Jobs         []ActionsRunDiagnosticJob `json:"jobs"`
}
type actionsRunDiagnosticReader interface {
	ObserveActionsRunAttempt(context.Context, string, int64, int64, int64) (reviewprovider.ActionsRunObservation, error)
}

func (e *Engine) ObserveActionsRunDiagnostic(ctx context.Context, scope core.Scope, runID, attempt int64, limit int) (ActionsRunDiagnostic, error) {
	if ctx == nil || e == nil || e.store == nil || runID <= 0 || attempt <= 0 || limit < 1 || limit > 20 {
		return ActionsRunDiagnostic{}, invalid()
	}
	answer, err := e.observeActionsRead(ctx, scope, 0, limit, runID, attempt)
	return answer.run, err
}
func actionsRunTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
func actionsRunState(status, conclusion string) bool {
	if status == "completed" {
		switch conclusion {
		case "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", "stale", "startup_failure":
			return true
		}
		return false
	}
	switch status {
	case "queued", "in_progress", "waiting", "pending", "requested":
		return conclusion == ""
	}
	return false
}
func actionsRunJobName(name string) bool {
	return name != "" && len(name) <= 255 && utf8.ValidString(name) && strings.TrimSpace(name) == name && strings.IndexFunc(name, unicode.IsControl) < 0
}
func actionsRunPriority(status string) int {
	if status == "in_progress" {
		return 0
	}
	if status == "completed" {
		return 2
	}
	return 1
}
func actionsRunJobTimes(status, conclusion string, start, end time.Time) bool {
	return (status != "in_progress" || !start.IsZero()) && ((status == "completed") == !end.IsZero()) && (start.IsZero() || end.IsZero() || !end.Before(start) || status == "completed" && (conclusion == "skipped" || conclusion == "cancelled"))
}
func projectActionsRunDiagnostic(request *actionsDiagnosticRequest, facts reviewprovider.ActionsRunObservation) (ActionsRunDiagnostic, error) {
	b, run := request.binding, facts.Run
	if run.RepositoryID != b.repositoryID || !strings.EqualFold(run.Repository, b.repository) || run.ID != request.runID || run.RunAttempt != request.attempt || run.ID <= 0 || run.CheckSuiteID <= 0 || run.WorkflowID <= 0 || run.RunNumber <= 0 || !sha.MatchString(run.HeadSHA) || !actionsDiagnosticEvent(run.Event) || !actionsRunState(run.Status, run.Conclusion) || run.CreatedAt.IsZero() || run.UpdatedAt.Before(run.CreatedAt) || run.UpdatedAt.After(facts.ObservedAt.Add(5*time.Second)) || facts.ObservedAt.Before(request.started) || facts.ObservedAt.After(time.Now().Add(5*time.Second)) || facts.ObservedAt.IsZero() || facts.Jobs == nil || len(facts.Jobs) > 800 {
		return ActionsRunDiagnostic{}, actionsDiagnosticUnavailable()
	}
	out := ActionsRunDiagnostic{ProjectID: b.project, Repository: b.repository, RepositoryID: b.repositoryID, ObservedAt: actionsRunTime(facts.ObservedAt), Run: ActionsRunDiagnosticRun{run.ID, run.CheckSuiteID, run.WorkflowID, run.RunNumber, run.RunAttempt, run.HeadSHA, run.Event, run.Status, run.Conclusion, actionsRunTime(run.CreatedAt), actionsRunTime(run.UpdatedAt)}, Limit: request.limit, Total: len(facts.Jobs), Capacity: "unknown", Completeness: "non_atomic_attempt_jobs", Jobs: []ActionsRunDiagnosticJob{}}
	ids, checks := map[int64]bool{}, map[int64]bool{}
	for _, job := range facts.Jobs {
		if job.ID <= 0 || job.CheckRunID <= 0 || ids[job.ID] || checks[job.CheckRunID] || job.HeadSHA != run.HeadSHA || !actionsRunJobName(job.Name) || !actionsRunState(job.Status, job.Conclusion) || !actionsRunJobTimes(job.Status, job.Conclusion, job.StartedAt, job.CompletedAt) || job.CompletedAt.After(facts.ObservedAt.Add(5*time.Second)) {
			return ActionsRunDiagnostic{}, actionsDiagnosticUnavailable()
		}
		ids[job.ID], checks[job.CheckRunID] = true, true
		switch job.Status {
		case "in_progress":
			out.Running++
		case "completed":
			out.Completed++
		default:
			out.Pending++
		}
		out.Jobs = append(out.Jobs, ActionsRunDiagnosticJob{job.ID, job.CheckRunID, job.Name, job.Status, job.Conclusion, actionsRunTime(job.StartedAt), actionsRunTime(job.CompletedAt)})
	}
	sort.Slice(out.Jobs, func(i, j int) bool {
		a, b := actionsRunPriority(out.Jobs[i].Status), actionsRunPriority(out.Jobs[j].Status)
		if a != b {
			return a < b
		}
		return out.Jobs[i].ID < out.Jobs[j].ID
	})
	if len(out.Jobs) > request.limit {
		out.Jobs = out.Jobs[:request.limit]
	}
	for {
		out.Omitted = out.Total - len(out.Jobs)
		raw, err := json.Marshal(out)
		if err != nil {
			return ActionsRunDiagnostic{}, actionsDiagnosticUnavailable()
		}
		if len(raw)+1 <= actionsDiagnosticBytes {
			return out, nil
		}
		if len(out.Jobs) == 0 {
			return ActionsRunDiagnostic{}, actionsDiagnosticUnavailable()
		}
		out.Jobs = out.Jobs[:len(out.Jobs)-1]
	}
}
