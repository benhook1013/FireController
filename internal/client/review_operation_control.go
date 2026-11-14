package client

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
)

// ReviewOperationWait retains the same request while no provider action is running.
// A retry deadline is the earliest automatic recheck, not a promised start time.
type ReviewOperationWait struct {
	CLIAllowance      *ReviewAllowanceObservation `json:"cli_allowance,omitempty"`
	BaselineAttemptID string                      `json:"baseline_attempt_id,omitempty"`
	RetryNotBefore    string                      `json:"retry_not_before,omitempty"`
	Paused            bool                        `json:"paused"`
}

func validReviewOperationWait(w *ReviewOperationWait, channel, state, attempt string) bool {
	if w == nil {
		return true
	}
	if state != "queued" || attempt != "" {
		return false
	}
	if w.CLIAllowance != nil {
		// These are the original exhausted baseline's facts, not new quota.
		baseline := ReviewOperation{Channel: channel, State: "held", Reason: "allowance_exhausted", AttemptID: w.BaselineAttemptID, Allowance: w.CLIAllowance}
		if channel != "cli" || !validReviewAllowance(baseline) {
			return false
		}
	}
	if w.BaselineAttemptID == "" {
		return w.RetryNotBefore == ""
	}
	return oneReviewOf(channel, "cli", "hosted") && validReviewOpaqueID(w.BaselineAttemptID, "review_attempt_") && validAllowanceTime(w.RetryNotBefore)
}

type ReviewOperationControl struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Action           string `json:"action"`
}

type ReviewOperationControlReceipt struct {
	ID             string `json:"id"`
	State          string `json:"state"`
	Revision       int64  `json:"revision"`
	Paused         bool   `json:"paused"`
	RetryNotBefore string `json:"retry_not_before,omitempty"`
}

func ValidateReviewOperationControl(id string, v ReviewOperationControl) error {
	if ValidateReviewOperationID(id) != nil || v.ExpectedRevision < 1 || v.ExpectedRevision == math.MaxInt64 || !oneReviewOf(v.Action, "pause", "resume", "cancel") {
		return mutationValidation()
	}
	return nil
}

// ControlReviewOperation only changes an undispatched queued intent.
// It never claims to cancel an issued provider request or changes review allowance.
func (c *Client) ControlReviewOperation(ctx context.Context, key, id string, v ReviewOperationControl) (ReviewOperationControlReceipt, error) {
	if !validMutationKey(key) || ValidateReviewOperationControl(id, v) != nil {
		return ReviewOperationControlReceipt{}, mutationValidation()
	}
	body, _ := json.Marshal(v)
	var out ReviewOperationControlReceipt
	if err := c.requestBounded(ctx, http.MethodPost, c.projectPath("reviews", "operations", id, "control"), nil, true, key, body, http.StatusOK, true, &out, 16<<10, decodeReviewResponse); err != nil {
		var failure *Error
		if errors.As(err, &failure) && failure.Code == "review_conflict" && failure.Status == http.StatusConflict {
			copy := *failure
			copy.Message = "This operation's revision changed or cannot advance."
			copy.Action = "Read this same operation's current state before deciding on a new control action; rereading cannot resolve a revision at the maximum value. To recover an unknown outcome, retain the original authenticated principal, project, worker, exact request body, operation ID and idempotency key rather than choosing a new action or key."
			return ReviewOperationControlReceipt{}, &copy
		}
		if errors.As(err, &failure) && failure.Code == "review_operation_started" && failure.Status == http.StatusConflict {
			copy := *failure
			copy.Message = "This operation is no longer an undispatched queued request."
			copy.Action = "Read this same operation for its current state. No provider cancellation occurred. Queued-only controls cannot reopen a held target_changed; follow its recovery diagnostic. Do not submit a replacement for posting, active or uncertain work."
			return ReviewOperationControlReceipt{}, &copy
		}
		return out, err
	}
	valid := out.ID == id && out.Revision == v.ExpectedRevision+1 && (out.RetryNotBefore == "" || validAllowanceTime(out.RetryNotBefore))
	switch v.Action {
	case "pause":
		valid = valid && out.State == "queued" && out.Paused
	case "resume":
		valid = valid && out.State == "queued" && !out.Paused
	case "cancel":
		valid = valid && out.State == "cancelled" && !out.Paused
	}
	if !valid {
		return ReviewOperationControlReceipt{}, unknownMutation("invalid_response", "The service returned invalid operation control facts; its outcome is unknown.", http.StatusOK)
	}
	return out, nil
}
