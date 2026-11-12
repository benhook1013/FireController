package reviewexec

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func (e *Engine) registerCIWatchHTTP(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/ci/watch", e.ciWatchHTTP)
}

func (e *Engine) ciWatchHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	w.Header().Add("Vary", "Prefer")
	w.Header().Add("Vary", "X-FC-CI-Watch")
	w.Header().Add("Vary", "X-FC-CI-Workflow-Details")
	watchVersion := r.Header.Get("X-FC-CI-Watch")
	changeAware := len(r.Header.Values("X-FC-CI-Watch")) == 1 && (watchVersion == "change-v1" || watchVersion == "change-v2" || watchVersion == "change-v3")
	workflowVersion := r.Header.Get("X-FC-CI-Workflow-Details")
	headAttention := changeAware && (watchVersion == "change-v2" || watchVersion == "change-v3") && len(r.Header.Values("X-FC-CI-Workflow-Details")) == 1 && (workflowVersion == "2" || workflowVersion == "3")
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, ciWatchRefusal(err, "ci_watch_scope_read"))
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	wait, err := operationPreference(r, 300)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	var input struct {
		Targets []struct {
			PullNumber          int64           `json:"pull_number"`
			HeadSHA             string          `json:"head_sha"`
			BaseSHA             json.RawMessage `json:"base_sha,omitempty"`
			AfterObservationKey json.RawMessage `json:"after_observation_key,omitempty"`
			FailureWorkflowIDs  json.RawMessage `json:"failure_workflow_ids,omitempty"`
		} `json:"targets"`
	}
	if err = core.ReadJSON(w, r, &input); err != nil {
		core.WriteError(w, err)
		return
	}
	targets := make([]CIWatchTarget, 0, len(input.Targets))
	for _, value := range input.Targets {
		base := ""
		if len(value.BaseSHA) > 0 && (json.Unmarshal(value.BaseSHA, &base) != nil || string(value.BaseSHA) == "null" || base == "") {
			core.WriteError(w, invalid())
			return
		}
		after := ""
		if len(value.AfterObservationKey) > 0 && (!changeAware || json.Unmarshal(value.AfterObservationKey, &after) != nil || string(value.AfterObservationKey) == "null" || after == "") {
			core.WriteError(w, invalid())
			return
		}
		var workflows []int64
		if len(value.FailureWorkflowIDs) > 0 && (!changeAware || json.Unmarshal(value.FailureWorkflowIDs, &workflows) != nil || workflows == nil || len(workflows) == 0) {
			core.WriteError(w, invalid())
			return
		}
		targets = append(targets, CIWatchTarget{AllowPassedRearm: changeAware && watchVersion == "change-v3", PullNumber: value.PullNumber, HeadSHA: value.HeadSHA, BaseSHA: base, AfterObservationKey: after, FailureWorkflowIDs: workflows, AllowHeadAttention: headAttention && len(workflows) > 0, AllowHistoricalAttention: headAttention && watchVersion == "change-v3" && workflowVersion == "3" && len(workflows) > 0})
	}
	if !validCIWatchTargets(targets) {
		core.WriteError(w, invalid())
		return
	}
	responseStarted := started
	if wait == 0 {
		// Immediate reads retain their ordinary response-write allowance.
		responseStarted = time.Now()
	}
	if err = operationResponseDeadlineAt(w, responseStarted.Add(time.Duration(wait+5)*time.Second)); err != nil {
		core.WriteError(w, err)
		return
	}
	w.Header().Set("Preference-Applied", "wait="+strconv.Itoa(wait))
	project := func(result CIWatchResult) CIWatchResult {
		if !changeAware {
			for i := range result.Targets {
				result.Targets[i].ObservationKey = ""
				result.Targets[i].WaitingForChange = false
			}
		}
		if !changeAware || watchVersion != "change-v2" && watchVersion != "change-v3" {
			for i := range result.Targets {
				result.Targets[i].MergeBaseMismatch = nil
			}
		}
		if len(r.Header.Values("X-FC-CI-Workflow-Details")) != 1 || workflowVersion != "1" && workflowVersion != "2" && workflowVersion != "3" {
			for i := range result.Targets {
				result.Targets[i].WorkflowObservations = nil
			}
		}
		return result
	}
	result, err := e.watchCIFromProjected(r.Context(), scope, targets, wait, started, project)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if changeAware && watchVersion == "change-v3" {
		w.Header().Set("X-FC-CI-Watch", "change-v3")
	}
	core.WriteJSON(w, http.StatusOK, result)
}
