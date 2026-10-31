package client

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"
)

type checkpointArchiveContent struct {
	ExpectedJobRevision int64    `json:"expected_job_revision"`
	Done                string   `json:"done"`
	Next                string   `json:"next"`
	Blocker             string   `json:"blocker"`
	Evidence            []string `json:"evidence"`
}

type Checkpoint struct {
	ID          string   `json:"id"`
	ProjectID   string   `json:"project_id"`
	JobID       string   `json:"job_id"`
	JobRevision int64    `json:"job_revision"`
	Sequence    int64    `json:"sequence"`
	ActorID     string   `json:"actor_id"`
	WorkerID    string   `json:"worker_id"`
	Done        string   `json:"done"`
	Next        string   `json:"next"`
	Blocker     string   `json:"blocker"`
	Evidence    []string `json:"evidence"`
	CreatedAt   string   `json:"created_at"`
}

func checkpointText(value string, max int, required bool) bool {
	if !utf8.ValidString(value) || len(value) > max || required && strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if r == 127 || r < 32 && r != '\t' && r != '\r' && r != '\n' {
			return false
		}
	}
	return true
}

func validateCheckpointArchiveContent(request checkpointArchiveContent) error {
	if request.ExpectedJobRevision < 1 {
		return fail("invalid_revision", "A positive expected job revision is required.", "Read the current job revision and supply it explicitly.", 400)
	}
	if !checkpointText(request.Done, 8<<10, true) || !checkpointText(request.Next, 8<<10, true) || !checkpointText(request.Blocker, 4<<10, false) || len(request.Evidence) > 10 {
		return mutationValidation()
	}
	size := len(request.Done) + len(request.Next) + len(request.Blocker)
	for _, evidence := range request.Evidence {
		if !checkpointText(evidence, 1024, true) {
			return mutationValidation()
		}
		size += len(evidence)
	}
	if size > 16<<10 {
		return mutationValidation()
	}
	return nil
}

func (c *Client) validCheckpoint(value Checkpoint, jobID string) bool {
	if !validID(value.ID) || value.ProjectID != c.identity.ProjectID || value.JobID != jobID || value.Sequence < 1 || !validID(value.ActorID) || value.WorkerID != "" && !validID(value.WorkerID) || value.Evidence == nil {
		return false
	}
	if len(value.CreatedAt) > 64 {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, value.CreatedAt); err != nil {
		return false
	}
	return validateCheckpointArchiveContent(checkpointArchiveContent{value.JobRevision, value.Done, value.Next, value.Blocker, value.Evidence}) == nil
}

func (c *Client) CheckpointHistory(ctx context.Context, id string, options PageOptions) (Page[Checkpoint], error) {
	query, err := pageQuery(options)
	if err != nil || !validID(id) {
		return Page[Checkpoint]{}, validation()
	}
	var value Page[Checkpoint]
	if err = c.get(ctx, c.projectPath("jobs", id, "checkpoints"), query, true, &value); err != nil {
		return Page[Checkpoint]{}, err
	}
	if !validPage(value, options) {
		return Page[Checkpoint]{}, invalidResponse()
	}
	var previous int64
	for _, checkpoint := range value.Items {
		if !c.validCheckpoint(checkpoint, id) || previous != 0 && checkpoint.Sequence >= previous {
			return Page[Checkpoint]{}, invalidResponse()
		}
		previous = checkpoint.Sequence
	}
	return value, nil
}
