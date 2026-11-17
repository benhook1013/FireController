package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const ActionsDiagnosticBudget = 25 * time.Second

type ActionsDiagnosticOptions struct {
	AgeHours int `json:"age_hours,omitempty"`
	Limit    int `json:"limit,omitempty"`
}
type ActionsDiagnosticRun struct {
	ID           int64  `json:"id"`
	CheckSuiteID int64  `json:"check_suite_id"`
	WorkflowID   int64  `json:"workflow_id"`
	RunNumber    int64  `json:"run_number"`
	RunAttempt   int64  `json:"run_attempt"`
	HeadSHA      string `json:"head_sha"`
	Event        string `json:"event"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type ActionsDiagnostic struct {
	ProjectID    string                 `json:"project_id"`
	Repository   string                 `json:"repository"`
	RepositoryID int64                  `json:"repository_id"`
	ObservedAt   string                 `json:"observed_at"`
	AgeHours     int                    `json:"age_hours"`
	Limit        int                    `json:"limit"`
	Scanned      int                    `json:"scanned"`
	Aged         int                    `json:"aged"`
	Omitted      int                    `json:"omitted"`
	Capacity     string                 `json:"capacity"`
	Completeness string                 `json:"completeness"`
	Runs         []ActionsDiagnosticRun `json:"runs"`
}

func ValidateActionsDiagnosticOptions(options ActionsDiagnosticOptions) error {
	if options.AgeHours < 0 || options.AgeHours > 720 || options.Limit < 0 || options.Limit > 20 {
		return validation()
	}
	return nil
}
func (c *Client) ActionsLiveness(ctx context.Context, options ActionsDiagnosticOptions) (ActionsDiagnostic, error) {
	if ValidateActionsDiagnosticOptions(options) != nil {
		return ActionsDiagnostic{}, validation()
	}
	if options.AgeHours == 0 {
		options.AgeHours = 24
	}
	if options.Limit == 0 {
		options.Limit = 10
	}
	body, err := json.Marshal(options)
	if err != nil {
		return ActionsDiagnostic{}, validation()
	}
	var out ActionsDiagnostic
	if err = c.requestWithTimeout(ctx, http.MethodPost, c.projectPath("reviews", "actions-liveness", "observe"), nil, true, "", body, http.StatusOK, false, &out, 8<<10, decodeReviewResponse, nil, ActionsDiagnosticBudget); err != nil {
		return ActionsDiagnostic{}, actionsDiagnosticFailure(err)
	}
	if !validActionsDiagnostic(out, c.identity.ProjectID, options) {
		return ActionsDiagnostic{}, invalidResponse()
	}
	return out, nil
}
func validActionsDiagnostic(out ActionsDiagnostic, project string, options ActionsDiagnosticOptions) bool {
	parts := strings.Split(out.Repository, "/")
	observed, err := time.Parse(time.RFC3339Nano, out.ObservedAt)
	if out.ProjectID != project || out.RepositoryID <= 0 || len(parts) != 2 || !hostedQueryRepositoryPart(parts[0]) || !hostedQueryRepositoryPart(parts[1]) || err != nil || observed.IsZero() || out.AgeHours != options.AgeHours || out.Limit != options.Limit || out.Scanned < 0 || out.Scanned > 1000 || out.Aged < 0 || out.Aged > out.Scanned || out.Omitted != out.Aged-len(out.Runs) || out.Omitted < 0 || out.Capacity != "unknown" || out.Completeness != "non_atomic_status_buckets" || out.Runs == nil || len(out.Runs) > options.Limit {
		return false
	}
	seen := map[int64]bool{}
	numbers := map[[2]int64]bool{}
	var previous time.Time
	var previousID int64
	for _, run := range out.Runs {
		created, a := time.Parse(time.RFC3339Nano, run.CreatedAt)
		updated, b := time.Parse(time.RFC3339Nano, run.UpdatedAt)
		if run.ID <= 0 || seen[run.ID] || numbers[[2]int64{run.WorkflowID, run.RunNumber}] || run.CheckSuiteID <= 0 || run.WorkflowID <= 0 || run.RunNumber <= 0 || run.RunAttempt <= 0 || !controlHex(run.HeadSHA, 40) || !actionsDiagnosticEvent(run.Event) || a != nil || b != nil || created.IsZero() || updated.Before(created) || updated.After(observed) || observed.Sub(updated) < time.Duration(options.AgeHours)*time.Hour || !previous.IsZero() && (updated.Before(previous) || updated.Equal(previous) && run.ID <= previousID) {
			return false
		}
		switch run.Status {
		case "queued", "in_progress", "waiting", "pending", "requested":
		default:
			return false
		}
		seen[run.ID] = true
		numbers[[2]int64{run.WorkflowID, run.RunNumber}] = true
		previous = updated
		previousID = run.ID
	}
	return true
}

func actionsDiagnosticEvent(value string) bool {
	if len(value) == 0 || len(value) > 64 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}

func actionsDiagnosticFailure(err error) error {
	var failure *Error
	if !errors.As(err, &failure) || failure.Status != http.StatusServiceUnavailable || (failure.Code != "actions_diagnostic_busy" && failure.Code != "actions_diagnostic_unavailable") {
		return err
	}
	copy := *failure
	copy.Message = "The explicit bounded Actions diagnostic did not establish a result."
	copy.Action = "Continue other work; if still useful later, explicitly request one new diagnostic. Do not repeatedly poll, infer run death/capacity, cancel or restart runs from this refusal. No automatic retry was attempted."
	return &copy
}
