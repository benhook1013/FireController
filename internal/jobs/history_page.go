package jobs

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

func jobHistoryUnavailable() error {
	return core.Fail("storage_unavailable", "Stored job history is unavailable.", http.StatusInternalServerError)
}

// Native snapshots before unified progress legitimately omit progress. Validate
// both supported shapes without changing their retained representation.
func validStoredJobHistory(item historyEntry, projectID, jobID string) bool {
	var fields map[string]json.RawMessage
	if rejectNullFields(item.State, &fields) != nil {
		return false
	}
	for _, name := range []string{"id", "project_id", "title", "brief", "worker_id", "status", "revision", "created_at", "updated_at"} {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	if raw, ok := fields["progress"]; ok {
		var p progress
		if rejectNullFields(raw, &p) != nil || !validProgress(p) {
			return false
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) != nil {
			return false
		}
		for _, name := range []string{"summary", "next", "blocker", "evidence"} {
			value, ok := nested[name]
			if !ok || strings.TrimSpace(string(value)) == "null" {
				return false
			}
		}
		delete(fields, "progress")
	}
	flat, err := json.Marshal(fields)
	var state job
	return err == nil && rejectNullFields(flat, &state) == nil && state.ID == jobID && state.ProjectID == projectID && state.Revision == item.Revision && validContextJob(state) && validPrimaryID(item.ActorID) && contextTime(item.CreatedAt)
}
