package jobs

import (
	"github.com/benhook1013/FireController/internal/core"
	"strings"
	"time"
	"unicode/utf8"
)

const maxJobContext = 1 << 20

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
