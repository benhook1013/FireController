package reviewexec

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/benhook1013/FireController/internal/core"
)

func (e *Engine) registerCIWatchHTTP(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/ci/watch", e.ciWatchHTTP)
}

func (e *Engine) ciWatchHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Prefer")
	w.Header().Add("Vary", "X-FC-CI-Watch")
	changeAware := len(r.Header.Values("X-FC-CI-Watch")) == 1 && r.Header.Get("X-FC-CI-Watch") == "change-v1"
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
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
		targets = append(targets, CIWatchTarget{PullNumber: value.PullNumber, HeadSHA: value.HeadSHA, BaseSHA: base, AfterObservationKey: after, FailureWorkflowIDs: workflows})
	}
	if !validCIWatchTargets(targets) {
		core.WriteError(w, invalid())
		return
	}
	if err = operationResponseDeadline(w, wait); err != nil {
		core.WriteError(w, err)
		return
	}
	w.Header().Set("Preference-Applied", "wait="+strconv.Itoa(wait))
	result, err := e.WatchCI(r.Context(), scope, targets, wait)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if !changeAware {
		for i := range result.Targets {
			result.Targets[i].ObservationKey = ""
			result.Targets[i].WaitingForChange = false
		}
	}
	core.WriteJSON(w, http.StatusOK, result)
}
