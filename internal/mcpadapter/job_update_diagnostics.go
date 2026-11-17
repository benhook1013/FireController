package mcpadapter

import (
	"encoding/json"
	"math"
	"strings"
)

func jobUpdateDiagnostic(field, requirement string) *Diagnostic {
	return diagnostic("invalid_arguments", field+": "+requirement, "Correct the job_update arguments. No request was sent; no profile, credential or job state was changed.")
}

// Diagnose a refused patch, never echoing authored text or undeclared keys.
// DecodeJobPatch remains the sole admission/normalization owner; valid calls
// take no extra diagnostic pass and retain their existing request semantics.
func invalidJobUpdatePatch(fields map[string]json.RawMessage) *Diagnostic {
	var revision int64
	if json.Unmarshal(fields["expected_revision"], &revision) != nil || revision < 1 || revision == math.MaxInt64 {
		return jobUpdateDiagnostic("expected_revision", "Use the job's current integer revision, at least 1 and below 9223372036854775807.")
	}
	for name := range fields {
		switch name {
		case "expected_revision", "title", "brief", "worker_id", "status", "progress":
		case "summary", "next", "blocker", "evidence":
			return jobUpdateDiagnostic("progress."+name, "Put this member inside the progress object, not at the top level.")
		default:
			return jobUpdateDiagnostic("job_update", "Only title, brief, worker_id, status and progress are editable; progress contains summary, next, blocker and evidence.")
		}
	}
	for _, rule := range []struct {
		name     string
		max      int
		nonblank bool
	}{{"title", 240, true}, {"brief", 32 << 10, false}, {"worker_id", 128, false}, {"status", 16, true}} {
		raw, present := fields[rule.name]
		if !present {
			continue
		}
		var value string
		if string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
			return jobUpdateDiagnostic(rule.name, "Use a string; omit unchanged fields rather than supplying null.")
		}
		measured := value
		if rule.name == "title" || rule.name == "worker_id" {
			measured = strings.TrimSpace(value)
		}
		if len(measured) > rule.max || rule.nonblank && strings.TrimSpace(value) == "" {
			switch rule.name {
			case "title":
				return jobUpdateDiagnostic("title", "Use nonblank text of at most 240 UTF-8 bytes.")
			case "brief":
				return jobUpdateDiagnostic("brief", "Use at most 32768 UTF-8 bytes; an empty string clears the brief.")
			case "worker_id":
				return jobUpdateDiagnostic("worker_id", "Use a complete worker ID of at most 128 bytes, or an empty string to unassign.")
			default:
				return jobUpdateDiagnostic("status", "Use active, parked, blocked or completed.")
			}
		}
		if rule.name == "worker_id" && measured != "" && !validID(measured) {
			return jobUpdateDiagnostic("worker_id", "Use the complete worker ID returned by the service, or an empty string to unassign.")
		}
		if rule.name == "status" && value != "active" && value != "parked" && value != "blocked" && value != "completed" {
			return jobUpdateDiagnostic("status", "Use active, parked, blocked or completed.")
		}
	}
	if raw, present := fields["progress"]; present {
		var progress map[string]json.RawMessage
		if json.Unmarshal(raw, &progress) != nil || len(progress) == 0 {
			return jobUpdateDiagnostic("progress", "Use a nonempty object containing only the changed summary, next, blocker or evidence members.")
		}
		total := 0
		for name := range progress {
			if name != "summary" && name != "next" && name != "blocker" && name != "evidence" {
				return jobUpdateDiagnostic("progress", "Only summary, next, blocker and evidence are supported; omit unchanged members.")
			}
		}
		for _, rule := range []struct {
			name string
			max  int
		}{{"summary", 8 << 10}, {"next", 8 << 10}, {"blocker", 4 << 10}} {
			value, present := progress[rule.name]
			if !present {
				continue
			}
			var text string
			if string(value) == "null" || json.Unmarshal(value, &text) != nil {
				return jobUpdateDiagnostic("progress."+rule.name, "Use a string; an empty string clears this member.")
			}
			if len(text) > rule.max {
				if rule.max == 4<<10 {
					return jobUpdateDiagnostic("progress.blocker", "Use at most 4096 UTF-8 bytes.")
				}
				return jobUpdateDiagnostic("progress."+rule.name, "Use at most 8192 UTF-8 bytes.")
			}
			for _, r := range text {
				if r == 127 || r < 32 && r != '\t' && r != '\n' && r != '\r' {
					return jobUpdateDiagnostic("progress."+rule.name, "Use text without control characters other than tab, carriage return and newline.")
				}
			}
			total += len(text)
		}
		if value, present := progress["evidence"]; present {
			var refs []string
			if json.Unmarshal(value, &refs) != nil || refs == nil || len(refs) > 10 {
				return jobUpdateDiagnostic("progress.evidence", "Use an array of at most 10 nonblank string references; [] clears it.")
			}
			for _, ref := range refs {
				if strings.TrimSpace(ref) == "" || len(ref) > 1024 {
					return jobUpdateDiagnostic("progress.evidence", "Each reference must be nonblank and at most 1024 UTF-8 bytes.")
				}
				total += len(ref)
			}
		}
		if total > 16<<10 {
			return jobUpdateDiagnostic("progress", "The supplied progress text and evidence together must fit within 16384 UTF-8 bytes.")
		}
	}
	return jobUpdateDiagnostic("job_update", "Supply at least one valid edit: title, brief, worker_id, status or the progress object. Text limits count UTF-8 bytes.")
}
