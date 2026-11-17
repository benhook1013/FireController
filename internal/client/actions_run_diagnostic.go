package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type ActionsRunDiagnosticOptions struct {
	RunID      int64 `json:"run_id"`
	RunAttempt int64 `json:"run_attempt"`
	Limit      int   `json:"limit,omitempty"`
}
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

func ValidateActionsRunDiagnosticOptions(o ActionsRunDiagnosticOptions) error {
	if o.RunID <= 0 || o.RunAttempt <= 0 || o.Limit < 0 || o.Limit > 20 {
		return validation()
	}
	return nil
}
func (c *Client) ActionsRun(ctx context.Context, o ActionsRunDiagnosticOptions) (ActionsRunDiagnostic, error) {
	if ValidateActionsRunDiagnosticOptions(o) != nil {
		return ActionsRunDiagnostic{}, validation()
	}
	if o.Limit == 0 {
		o.Limit = 10
	}
	body, err := json.Marshal(o)
	if err != nil {
		return ActionsRunDiagnostic{}, validation()
	}
	var out ActionsRunDiagnostic
	err = c.requestWithTimeout(ctx, http.MethodPost, c.projectPath("reviews", "actions-run", "observe"), nil, true, "", body, http.StatusOK, false, &out, 8<<10, decodeReviewResponse, nil, ActionsDiagnosticBudget)
	if err != nil {
		return ActionsRunDiagnostic{}, actionsDiagnosticFailure(err)
	}
	if !validActionsRunDiagnostic(out, c.identity.ProjectID, o) {
		return ActionsRunDiagnostic{}, invalidResponse()
	}
	return out, nil
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
func actionsRunPriority(status string) int {
	if status == "in_progress" {
		return 0
	}
	if status == "completed" {
		return 2
	}
	return 1
}
func actionsRunJobName(name string) bool {
	return name != "" && len(name) <= 255 && utf8.ValidString(name) && strings.TrimSpace(name) == name && strings.IndexFunc(name, unicode.IsControl) < 0
}
func actionsRunOptionalTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, value)
}
func validActionsRunDiagnostic(out ActionsRunDiagnostic, project string, o ActionsRunDiagnosticOptions) bool {
	parts := strings.Split(out.Repository, "/")
	observed, err := time.Parse(time.RFC3339Nano, out.ObservedAt)
	if out.ProjectID != project || out.RepositoryID <= 0 || len(parts) != 2 || !hostedQueryRepositoryPart(parts[0]) || !hostedQueryRepositoryPart(parts[1]) || err != nil || observed.IsZero() || out.Limit != o.Limit || out.Total < 0 || out.Total > 800 || out.Pending < 0 || out.Running < 0 || out.Completed < 0 || out.Pending+out.Running+out.Completed != out.Total || out.Jobs == nil || len(out.Jobs) > o.Limit || out.Omitted != out.Total-len(out.Jobs) || out.Omitted < 0 || out.Capacity != "unknown" || out.Completeness != "non_atomic_attempt_jobs" {
		return false
	}
	run := out.Run
	created, a := time.Parse(time.RFC3339Nano, run.CreatedAt)
	updated, b := time.Parse(time.RFC3339Nano, run.UpdatedAt)
	if run.ID != o.RunID || run.RunAttempt != o.RunAttempt || run.CheckSuiteID <= 0 || run.WorkflowID <= 0 || run.RunNumber <= 0 || !controlHex(run.HeadSHA, 40) || !actionsDiagnosticEvent(run.Event) || !actionsRunState(run.Status, run.Conclusion) || a != nil || b != nil || created.IsZero() || updated.Before(created) || updated.After(observed.Add(5*time.Second)) {
		return false
	}
	ids, checks := map[int64]bool{}, map[int64]bool{}
	pending, running, completed := 0, 0, 0
	previousPriority, previousID := -1, int64(0)
	for _, job := range out.Jobs {
		start, a := actionsRunOptionalTime(job.StartedAt)
		end, b := actionsRunOptionalTime(job.CompletedAt)
		priority := actionsRunPriority(job.Status)
		if job.ID <= 0 || job.CheckRunID <= 0 || ids[job.ID] || checks[job.CheckRunID] || !actionsRunJobName(job.Name) || !actionsRunState(job.Status, job.Conclusion) || a != nil || b != nil || job.Status == "in_progress" && start.IsZero() || (job.Status == "completed") != !end.IsZero() || !start.IsZero() && !end.IsZero() && end.Before(start) && !(job.Status == "completed" && (job.Conclusion == "skipped" || job.Conclusion == "cancelled")) || end.After(observed.Add(5*time.Second)) || priority < previousPriority || priority == previousPriority && job.ID <= previousID {
			return false
		}
		ids[job.ID], checks[job.CheckRunID] = true, true
		previousPriority, previousID = priority, job.ID
		switch job.Status {
		case "in_progress":
			running++
		case "completed":
			completed++
		default:
			pending++
		}
	}
	return pending <= out.Pending && running <= out.Running && completed <= out.Completed
}
