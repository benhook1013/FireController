package client

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"unicode/utf8"
)

func validMutationKey(key string) bool {
	if key == "" || len(key) > 128 || strings.TrimSpace(key) != key {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x20 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

// ValidMutationKey checks the canonical explicit-key rules before transport I/O.
func ValidMutationKey(key string) bool { return validMutationKey(key) }

// ValidStatus admits the current job states without normalization or defaults.
func ValidStatus(status string) bool { return validStatus(status) }

func mutationValidation() error {
	return fail("validation_failed", "The mutation requires valid bounded fields and an explicit idempotency key.", "Correct the request and provide one printable ASCII key of at most 128 bytes.", 400)
}

func (c *Client) mutate(ctx context.Context, method string, segments []string, key string, request any, status int, receipt any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return mutationValidation()
	}
	return c.request(ctx, method, segments, nil, true, key, body, status, true, receipt)
}

func (c *Client) CreateJob(ctx context.Context, key string, request CreateJobRequest) (JobReceipt, error) {
	if !validMutationKey(key) {
		return JobReceipt{}, mutationValidation()
	}
	request.Title = strings.TrimSpace(request.Title)
	request.WorkerID = strings.TrimSpace(request.WorkerID)
	if request.Status == "" {
		request.Status = "active"
	}
	if !utf8.ValidString(request.Title) || !utf8.ValidString(request.Brief) || request.Title == "" || len(request.Title) > 240 || len(request.Brief) > 32<<10 || request.WorkerID != "" && !validID(request.WorkerID) || !validStatus(request.Status) {
		return JobReceipt{}, mutationValidation()
	}
	var result JobReceipt
	if err := c.mutate(ctx, http.MethodPost, c.projectPath("jobs"), key, request, http.StatusCreated, &result); err != nil {
		return JobReceipt{}, err
	}
	if !validID(result.ID) || result.Revision != 1 || result.Outcome != "created" {
		return JobReceipt{}, unknownMutation("invalid_response", "The service returned an invalid job receipt; its outcome is unknown.", http.StatusCreated)
	}
	return result, nil
}

func (c *Client) EditJob(ctx context.Context, key, id string, request EditJobRequest) (JobReceipt, error) {
	if !validMutationKey(key) || !validID(id) || request.ExpectedRevision < 1 || request.ExpectedRevision == math.MaxInt64 || request.Title == nil && request.Brief == nil && request.WorkerID == nil && request.Status == nil {
		return JobReceipt{}, mutationValidation()
	}
	if request.Title != nil {
		value := strings.TrimSpace(*request.Title)
		if !utf8.ValidString(value) || value == "" || len(value) > 240 {
			return JobReceipt{}, mutationValidation()
		}
		request.Title = &value
	}
	if request.Brief != nil && (!utf8.ValidString(*request.Brief) || len(*request.Brief) > 32<<10) {
		return JobReceipt{}, mutationValidation()
	}
	if request.WorkerID != nil {
		value := strings.TrimSpace(*request.WorkerID)
		if value != "" && !validID(value) {
			return JobReceipt{}, mutationValidation()
		}
		request.WorkerID = &value
	}
	if request.Status != nil && !validStatus(*request.Status) {
		return JobReceipt{}, mutationValidation()
	}
	var result JobReceipt
	if err := c.mutate(ctx, http.MethodPatch, c.projectPath("jobs", id), key, request, http.StatusOK, &result); err != nil {
		return JobReceipt{}, err
	}
	if result.ID != id || !validID(result.ID) || result.Revision != request.ExpectedRevision+1 || result.Outcome != "updated" {
		return JobReceipt{}, unknownMutation("invalid_response", "The service returned an invalid job receipt; its outcome is unknown.", http.StatusOK)
	}
	return result, nil
}

func (c *Client) SendMessage(ctx context.Context, key string, request SendMessageRequest) (MessageReceipt, error) {
	if !validMutationKey(key) || !validID(request.RecipientWorkerID) || request.ReplyToMessageID != "" && !validID(request.ReplyToMessageID) || !utf8.ValidString(request.Body) || strings.TrimSpace(request.Body) == "" || len(request.Body) > 16<<10 {
		return MessageReceipt{}, mutationValidation()
	}
	var result MessageReceipt
	if err := c.mutate(ctx, http.MethodPost, c.projectPath("messages"), key, request, http.StatusCreated, &result); err != nil {
		return MessageReceipt{}, err
	}
	if !validID(result.ID) || !validID(result.ConversationID) || result.Outcome != "sent" {
		return MessageReceipt{}, unknownMutation("invalid_response", "The service returned an invalid message receipt; its outcome is unknown.", http.StatusCreated)
	}
	return result, nil
}

func (c *Client) MarkSeen(ctx context.Context, key, id string) (MessageReceipt, error) {
	return c.transition(ctx, key, id, "seen")
}

func (c *Client) Acknowledge(ctx context.Context, key, id string) (MessageReceipt, error) {
	return c.transition(ctx, key, id, "ack")
}

func (c *Client) transition(ctx context.Context, key, id, action string) (MessageReceipt, error) {
	if !validMutationKey(key) || !validID(id) {
		return MessageReceipt{}, mutationValidation()
	}
	var result MessageReceipt
	path := c.projectPath("messages", id, action)
	if err := c.mutate(ctx, http.MethodPost, path, key, struct{}{}, http.StatusOK, &result); err != nil {
		return MessageReceipt{}, err
	}
	want := action
	if action == "ack" {
		want = "acknowledged"
	}
	if result.ID != id || !validID(result.ConversationID) || result.Outcome != want && result.Outcome != "unchanged" {
		return MessageReceipt{}, unknownMutation("invalid_response", "The service returned an invalid message receipt; its outcome is unknown.", http.StatusOK)
	}
	return result, nil
}
