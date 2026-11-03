package client

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/benhook1013/FireController/internal/strictjson"
)

type WorkerRetirementBlocker struct {
	Count int64    `json:"count"`
	IDs   []string `json:"ids"`
}
type WorkerRetirementBlockers struct {
	Jobs     WorkerRetirementBlocker `json:"jobs"`
	Notes    WorkerRetirementBlocker `json:"notes"`
	Messages WorkerRetirementBlocker `json:"messages"`
}
type WorkerRetirement struct {
	WorkerID  string                   `json:"worker_id"`
	Retired   bool                     `json:"retired"`
	CanRetire bool                     `json:"can_retire"`
	Blockers  WorkerRetirementBlockers `json:"blockers"`
}
type WorkerRetirementReceipt struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}

func validWorkerRetirementBlockers(b WorkerRetirementBlockers) bool {
	for _, group := range []WorkerRetirementBlocker{b.Jobs, b.Notes, b.Messages} {
		if group.Count < 0 || group.Count >= 1<<53 || group.IDs == nil || len(group.IDs) != int(min(group.Count, 20)) {
			return false
		}
		seen := map[string]bool{}
		for _, id := range group.IDs {
			if !validID(id) || seen[id] {
				return false
			}
			seen[id] = true
		}
	}
	return true
}
func (c *Client) WorkerRetirement(ctx context.Context, worker string) (WorkerRetirement, error) {
	if !validID(worker) {
		return WorkerRetirement{}, validation()
	}
	var value WorkerRetirement
	if err := c.get(ctx, c.projectPath("workers", worker, "retirement"), nil, true, &value); err != nil {
		return WorkerRetirement{}, err
	}
	empty := value.Blockers.Jobs.Count == 0 && value.Blockers.Notes.Count == 0 && value.Blockers.Messages.Count == 0
	if value.WorkerID != worker || !validWorkerRetirementBlockers(value.Blockers) || value.CanRetire != (!value.Retired && empty) {
		return WorkerRetirement{}, invalidResponse()
	}
	return value, nil
}
func (c *Client) RetireWorker(ctx context.Context, key, worker string) (WorkerRetirementReceipt, error) {
	if !validID(worker) || !validMutationKey(key) {
		return WorkerRetirementReceipt{}, mutationValidation()
	}
	var value WorkerRetirementReceipt
	if err := c.mutate(ctx, http.MethodPost, c.projectPath("workers", worker, "retire"), key, struct{}{}, http.StatusOK, &value); err != nil {
		return WorkerRetirementReceipt{}, err
	}
	if value.ID != worker || value.Revision != 2 || !oneReviewOf(value.Outcome, "retired", "unchanged") {
		return WorkerRetirementReceipt{}, unknownMutation("invalid_response", "The worker retirement receipt was invalid; its outcome is unknown.", http.StatusOK)
	}
	return value, nil
}

func safeWorkerRetirementBlockers(value any) any {
	body, err := json.Marshal(value)
	var blockers WorkerRetirementBlockers
	if err != nil || strictjson.Decode(body, &blockers) != nil || !completeShape(body, &blockers) || !validWorkerRetirementBlockers(blockers) {
		return nil
	}
	return blockers
}
