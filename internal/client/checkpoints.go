package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/strictjson"
)

type AppendCheckpointRequest struct {
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

type CheckpointReceipt struct {
	ID          string `json:"id"`
	Sequence    int64  `json:"sequence"`
	JobRevision int64  `json:"job_revision"`
	Outcome     string `json:"outcome"`
}

type LatestCheckpointResult struct {
	JobID       string      `json:"job_id"`
	JobRevision int64       `json:"job_revision"`
	Checkpoint  *Checkpoint `json:"checkpoint"`
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

// ValidateCheckpointRequest is local admission only, shared by transports so
// malformed content is rejected before opening a credential or connection.
func ValidateCheckpointRequest(request AppendCheckpointRequest) error {
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

func (c *Client) AppendCheckpoint(ctx context.Context, key, id string, request AppendCheckpointRequest) (CheckpointReceipt, error) {
	if !validMutationKey(key) || !validID(id) {
		return CheckpointReceipt{}, mutationValidation()
	}
	if err := ValidateCheckpointRequest(request); err != nil {
		return CheckpointReceipt{}, err
	}
	if request.Evidence == nil {
		request.Evidence = []string{}
	}
	var result CheckpointReceipt
	if err := c.mutate(ctx, http.MethodPost, c.projectPath("jobs", id, "checkpoints"), key, request, http.StatusCreated, &result); err != nil {
		return CheckpointReceipt{}, err
	}
	if !validID(result.ID) || result.Sequence < 1 || result.JobRevision != request.ExpectedJobRevision || result.Outcome != "created" {
		return CheckpointReceipt{}, unknownMutation("invalid_response", "The service returned an invalid checkpoint receipt; its outcome is unknown.", http.StatusCreated)
	}
	return result, nil
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
	return ValidateCheckpointRequest(AppendCheckpointRequest{value.JobRevision, value.Done, value.Next, value.Blocker, value.Evidence}) == nil
}

func (c *Client) LatestCheckpoint(ctx context.Context, id string) (LatestCheckpointResult, error) {
	if !validID(id) {
		return LatestCheckpointResult{}, validation()
	}
	var value LatestCheckpointResult
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("jobs", id, "checkpoints", "latest"), nil, true, "", nil, http.StatusOK, false, &value, 128<<10, decodeLatestCheckpoint); err != nil {
		return LatestCheckpointResult{}, err
	}
	if value.JobID != id || value.JobRevision < 1 {
		return LatestCheckpointResult{}, invalidResponse()
	}
	if value.Checkpoint != nil && (!c.validCheckpoint(*value.Checkpoint, id) || value.Checkpoint.JobRevision > value.JobRevision) {
		return LatestCheckpointResult{}, invalidResponse()
	}
	return value, nil
}

func decodeLatestCheckpoint(data []byte, dst any) error {
	value, ok := dst.(*LatestCheckpointResult)
	if !ok || strictjson.Validate(data) != nil {
		return strictjson.ErrSyntax
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 3 || !completeShape(data, &struct {
		JobID       string `json:"job_id"`
		JobRevision int64  `json:"job_revision"`
	}{}) {
		return strictjson.ErrSyntax
	}
	checkpoint, present := fields["checkpoint"]
	if !present || json.Unmarshal(fields["job_id"], &value.JobID) != nil || json.Unmarshal(fields["job_revision"], &value.JobRevision) != nil {
		return strictjson.ErrSyntax
	}
	value.Checkpoint = nil
	if !bytes.Equal(bytes.TrimSpace(checkpoint), []byte("null")) {
		var full Checkpoint
		if decodeResponse(checkpoint, &full) != nil {
			return strictjson.ErrSyntax
		}
		value.Checkpoint = &full
	}
	return nil
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
