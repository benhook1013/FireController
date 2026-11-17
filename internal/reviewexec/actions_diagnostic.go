package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const actionsDiagnosticBytes = 8 << 10
const actionsDiagnosticBudget = 20 * time.Second

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

type actionsDiagnosticBinding struct {
	project, repository, capability, fingerprint, account string
	repositoryID, revision                                int64
}
type actionsDiagnosticRequest struct {
	started    time.Time
	ctx        context.Context
	scope      core.Scope
	binding    actionsDiagnosticBinding
	age, limit int
	result     chan actionsDiagnosticAnswer
}
type actionsDiagnosticAnswer struct {
	facts ActionsDiagnostic
	err   error
}
type actionsDiagnosticReader interface {
	ObserveActionsLiveness(context.Context, string, int64, time.Duration) (reviewprovider.ActionsLiveness, error)
}

func actionsDiagnosticUnavailable() error {
	return core.Fail("actions_diagnostic_unavailable", "Explicit Actions diagnostics are unavailable; no diagnostic or recovery result was established.", 503)
}
func actionsDiagnosticBusy() error {
	return core.Fail("actions_diagnostic_busy", "The bounded source workers or this project's source read are busy; no extra provider work was started.", 503)
}
func (e *Engine) actionsBinding(ctx context.Context, scope core.Scope) (actionsDiagnosticBinding, bool, error) {
	var b actionsDiagnosticBinding
	var busy bool
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var held bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metadata WHERE (key='restore:provider_state' AND value='held') OR (key='restore:state' AND value='inactive'))`).Scan(&held); err != nil {
			return err
		}
		if held {
			return actionsDiagnosticUnavailable()
		}
		c, revision, err := readConfiguration(ctx, tx, scope.ProjectID)
		if err != nil {
			return err
		}
		cap, ok := e.capabilities[c.CapabilityID]
		if !ok || e.validateConfiguration(c) != nil {
			return actionsDiagnosticUnavailable()
		}
		b = actionsDiagnosticBinding{project: scope.ProjectID, capability: c.CapabilityID, fingerprint: cap.Fingerprint, account: cap.Provider + "\x00" + cap.Account, revision: revision}
		if err = tx.QueryRowContext(ctx, `SELECT github_repository_id,repository FROM projects WHERE id=?`, scope.ProjectID).Scan(&b.repositoryID, &b.repository); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, sourceRefreshProjectBusy, scope.ProjectID).Scan(&busy)
	})
	if err == nil && (b.repositoryID <= 0 || !repository.MatchString(b.repository)) {
		err = actionsDiagnosticUnavailable()
	}
	return b, busy, err
}

// ObserveActionsDiagnostic is a one-shot safe read, not a durable action. Only
// the existing source-refresh workers perform provider work. No retry is queued.
func (e *Engine) ObserveActionsDiagnostic(ctx context.Context, scope core.Scope, age, limit int) (ActionsDiagnostic, error) {
	if ctx == nil || e == nil || e.store == nil || age < 1 || age > 720 || limit < 1 || limit > 20 {
		return ActionsDiagnostic{}, invalid()
	}
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(ctx, actionsDiagnosticBudget)
	defer cancel()
	binding, _, err := e.actionsBinding(ctx, scope)
	if err != nil {
		return ActionsDiagnostic{}, err
	}
	request := &actionsDiagnosticRequest{started: started, ctx: ctx, scope: scope, binding: binding, age: age, limit: limit, result: make(chan actionsDiagnosticAnswer, 1)}
	e.actionsDiagnosticMu.Lock()
	if !e.actionsDiagnosticActive {
		e.actionsDiagnosticMu.Unlock()
		return ActionsDiagnostic{}, actionsDiagnosticUnavailable()
	}
	if e.actionsDiagnosticCurrent != nil {
		e.actionsDiagnosticMu.Unlock()
		return ActionsDiagnostic{}, actionsDiagnosticBusy()
	}
	e.actionsDiagnosticCurrent = request
	e.actionsDiagnosticQueue <- request // one global request, one buffered slot
	e.actionsDiagnosticMu.Unlock()
	select {
	case <-ctx.Done():
		return ActionsDiagnostic{}, actionsDiagnosticUnavailable()
	case answer := <-request.result:
		if answer.err != nil {
			return ActionsDiagnostic{}, answer.err
		}
		current, _, err := e.actionsBinding(ctx, scope)
		if err != nil {
			return ActionsDiagnostic{}, err
		}
		if current != binding {
			return ActionsDiagnostic{}, actionsDiagnosticUnavailable()
		}
		return answer.facts, nil
	}
}
func (e *Engine) startActionsDiagnostics() {
	e.actionsDiagnosticMu.Lock()
	defer e.actionsDiagnosticMu.Unlock()
	if e.actionsDiagnosticQueue == nil {
		e.actionsDiagnosticQueue = make(chan *actionsDiagnosticRequest, 1)
	}
	e.actionsDiagnosticActive = true
}
func (e *Engine) stopActionsDiagnostics() {
	e.actionsDiagnosticMu.Lock()
	e.actionsDiagnosticActive = false
	request := e.actionsDiagnosticCurrent
	select {
	case <-e.actionsDiagnosticQueue:
	default:
	}
	e.actionsDiagnosticMu.Unlock()
	if request != nil {
		e.finishActionsDiagnostic(request, actionsDiagnosticAnswer{err: actionsDiagnosticUnavailable()})
	}
}
func (e *Engine) finishActionsDiagnostic(request *actionsDiagnosticRequest, answer actionsDiagnosticAnswer) {
	e.actionsDiagnosticMu.Lock()
	if e.actionsDiagnosticCurrent == request {
		e.actionsDiagnosticCurrent = nil
		e.actionsDiagnosticProject = ""
	}
	e.actionsDiagnosticMu.Unlock()
	select {
	case request.result <- answer:
	default:
	}
}
func (e *Engine) collectActionsDiagnostic(worker context.Context, readers map[string]TargetReader, schedule *sourceRefreshSchedule, request *actionsDiagnosticRequest) {
	answer := actionsDiagnosticAnswer{err: actionsDiagnosticUnavailable()}
	defer func() { e.finishActionsDiagnostic(request, answer) }()
	ctx, cancel := context.WithCancel(request.ctx)
	defer cancel()
	stop := context.AfterFunc(worker, cancel)
	defer stop()
	if ctx.Err() != nil {
		return
	}
	select {
	case schedule.claim <- struct{}{}:
	case <-ctx.Done():
		return
	}
	binding, busy, err := e.actionsBinding(ctx, request.scope)
	if err == nil && binding == request.binding && !busy && ctx.Err() == nil {
		e.actionsDiagnosticMu.Lock()
		e.actionsDiagnosticProject = binding.project
		e.actionsDiagnosticMu.Unlock()
	}
	<-schedule.claim
	if err != nil {
		answer.err = err
		return
	}
	if busy {
		answer.err = actionsDiagnosticBusy()
		return
	}
	if binding != request.binding || ctx.Err() != nil {
		return
	}
	reader, ok := readers[binding.capability].(actionsDiagnosticReader)
	if !ok {
		return
	}
	facts, err := reader.ObserveActionsLiveness(reviewprovider.WithGitHubCaller(ctx, "source_refresh"), binding.repository, binding.repositoryID, time.Duration(request.age)*time.Hour)
	if err != nil {
		reason := "provider_unavailable"
		var failure *reviewprovider.Error
		if errors.As(err, &failure) {
			switch failure.Code {
			case "provider_rate_limited", "provider_forbidden", "provider_not_found", "provider_invalid_response":
				reason = failure.Code
			}
		}
		answer.err = &core.Error{Code: "actions_diagnostic_unavailable", Message: "The bounded provider diagnostic failed; no partial facts or recovery permission were retained.", Status: 503, Details: map[string]string{"reason": reason}}
		return
	}
	if ctx.Err() != nil {
		return
	}
	answer.facts, answer.err = projectActionsDiagnostic(request, facts)
}
func projectActionsDiagnostic(request *actionsDiagnosticRequest, facts reviewprovider.ActionsLiveness) (ActionsDiagnostic, error) {
	b := request.binding
	if facts.RepositoryID != b.repositoryID || !strings.EqualFold(facts.Repository, b.repository) || facts.Runs == nil || facts.ObservedAt.IsZero() || facts.ObservedAt.Before(request.started) || facts.ObservedAt.After(time.Now().Add(5*time.Second)) || len(facts.Runs) > 1000 {
		return ActionsDiagnostic{}, actionsDiagnosticUnavailable()
	}
	out := ActionsDiagnostic{ProjectID: b.project, Repository: b.repository, RepositoryID: b.repositoryID, ObservedAt: facts.ObservedAt.UTC().Format(time.RFC3339Nano), AgeHours: request.age, Limit: request.limit, Scanned: len(facts.Runs), Capacity: "unknown", Completeness: "non_atomic_status_buckets", Runs: []ActionsDiagnosticRun{}}
	seen := map[int64]bool{}
	numbers := map[[2]int64]bool{}
	for _, run := range facts.Runs {
		if run.RepositoryID != b.repositoryID || !strings.EqualFold(run.Repository, b.repository) || run.ID <= 0 || seen[run.ID] || numbers[[2]int64{run.WorkflowID, run.RunNumber}] || !actionsDiagnosticEvent(run.Event) || run.CheckSuiteID <= 0 || run.WorkflowID <= 0 || run.RunNumber <= 0 || run.RunAttempt <= 0 || !sha.MatchString(run.HeadSHA) || run.Capacity != "unknown" || run.Conclusion != "" || run.CreatedAt.IsZero() || run.UpdatedAt.Before(run.CreatedAt) || run.UpdatedAt.After(facts.ObservedAt) {
			return ActionsDiagnostic{}, actionsDiagnosticUnavailable()
		}
		switch run.Status {
		case "queued", "in_progress", "waiting", "pending", "requested":
		default:
			return ActionsDiagnostic{}, actionsDiagnosticUnavailable()
		}
		seen[run.ID] = true
		numbers[[2]int64{run.WorkflowID, run.RunNumber}] = true
		if run.Aged && facts.ObservedAt.Sub(run.UpdatedAt) < time.Duration(request.age)*time.Hour {
			return ActionsDiagnostic{}, actionsDiagnosticUnavailable()
		}
		if run.Aged {
			out.Aged++
			out.Runs = append(out.Runs, ActionsDiagnosticRun{run.ID, run.CheckSuiteID, run.WorkflowID, run.RunNumber, run.RunAttempt, run.HeadSHA, run.Event, run.Status, run.CreatedAt.UTC().Format(time.RFC3339Nano), run.UpdatedAt.UTC().Format(time.RFC3339Nano)})
		}
	}
	sort.Slice(out.Runs, func(i, j int) bool {
		left, _ := time.Parse(time.RFC3339Nano, out.Runs[i].UpdatedAt)
		right, _ := time.Parse(time.RFC3339Nano, out.Runs[j].UpdatedAt)
		if left.Equal(right) {
			return out.Runs[i].ID < out.Runs[j].ID
		}
		return left.Before(right)
	})
	if len(out.Runs) > request.limit {
		out.Runs = out.Runs[:request.limit]
	}
	for {
		out.Omitted = out.Aged - len(out.Runs)
		raw, err := json.Marshal(out)
		if err != nil {
			return ActionsDiagnostic{}, actionsDiagnosticUnavailable()
		}
		if len(raw)+1 <= actionsDiagnosticBytes {
			return out, nil
		}
		if len(out.Runs) == 0 {
			return ActionsDiagnostic{}, actionsDiagnosticUnavailable()
		}
		out.Runs = out.Runs[:len(out.Runs)-1]
	}
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
