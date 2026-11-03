package client

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// Private response metadata decorates agent guidance, never the service body or
// admission decision. Foreign project identity is deliberately not represented.
type queuedReservation struct {
	Kind        string `json:"kind"`
	OperationID string `json:"operation_id,omitempty"`
	PullNumber  int64  `json:"pull_number,omitempty"`
	Channel     string `json:"channel,omitempty"`
	State       string `json:"state,omitempty"`
}

func readQueuedReservation(header http.Header, cause string) *queuedReservation {
	values := header.Values("X-FC-Review-Reservation")
	if cause != "reservation_busy" || len(values) != 1 || len(values[0]) > 512 {
		return nil
	}
	var v queuedReservation
	if strictjson.Decode([]byte(values[0]), &v) != nil {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(values[0]), &fields) != nil {
		return nil
	}
	switch v.Kind {
	case "same_project":
		if len(fields) != 5 || ValidateReviewOperationID(v.OperationID) != nil || !validPullNumber(v.PullNumber) || !oneReviewOf(v.Channel, "cli", "hosted") || !oneReviewOf(v.State, "preparing", "posting", "active", "uncertain") {
			return nil
		}
	case "other_project", "reservation_unproven":
		if len(fields) != 1 {
			return nil
		}
	default:
		return nil
	}
	return &v
}

func boundQueuedReservation(v *queuedReservation, operationID, channel string) *queuedReservation {
	if v != nil && v.Kind == "same_project" && (v.OperationID == operationID || channel != "" && v.Channel != channel) {
		return nil
	}
	return v
}

func recognizedQueuedCause(value string) string {
	switch value {
	case "operation_paused", "project_paused", "cooldown", "cli_quota_wait", "queue_order", "reservation_busy", "awaiting_scheduler":
		return value
	}
	return ""
}
func queuedOperationDiagnostic(cause string, reservation ...*queuedReservation) *ReviewOperationDiagnostic {
	var explanation string
	switch cause {
	case "operation_paused":
		explanation = "This queued operation is explicitly paused."
	case "project_paused":
		explanation = "Project execution is paused."
	case "cooldown":
		explanation = "This queued operation’s cooldown deadline is still in the future."
	case "cli_quota_wait":
		return &ReviewOperationDiagnostic{"Included CLI quota was exhausted on this shared provider account. This request remains queued while the controller uses bounded backoff across projects before checking again. The next available slot and provider reset are unknown.", "Keep this original request queued and continue other work. The controller rechecks the pinned comparison and all normal guards before starting; pause/cancel the undispatched request if needed. Do not submit a duplicate request, reset rounds or change accounts to bypass this wait."}
	case "queue_order":
		explanation = "An earlier applicable review request in this project’s PR order is queued, possibly on a future timer."
	case "reservation_busy":
		explanation = "A matching provider-account or target reservation is occupied. A due timer does not release it."
		if len(reservation) == 1 && reservation[0] != nil {
			holder := reservation[0]
			switch holder.Kind {
			case "same_project":
				explanation = fmt.Sprintf("One matching reservation is held by %s for PR #%d (%s, %s) in this project. A due timer does not release it.", holder.OperationID, holder.PullNumber, holder.Channel, holder.State)
			case "other_project":
				explanation = "A matching reservation is held in another project. Its private details are not exposed. A due timer does not release it."
			case "reservation_unproven":
				explanation = "A matching reservation exists, but its current operation binding could not be verified. This does not establish a stale reservation or permission to release it."
			}
		}
	case "awaiting_scheduler":
		explanation = "Queued for scheduler assessment; worker activity and provider availability are not established."
	default:
		return nil
	}
	action := "Keep the original request for unchanged intent. For an authorized change, see queued-only review_operation_control cancellation; respect any known cooldown because a replacement does not inherit its timer. A due timer is eligibility, not a start ETA. Normal admission and provider checks still apply."
	if cause == "reservation_busy" {
		action = "Keep this original request queued while the reservation holder is handled through its normal lifecycle; continue other work. Read a named same-project operation if its details are needed. This hint does not authorize cancellation, reservation release or changed review grants. A due timer is not a reserved start time."
	}
	return &ReviewOperationDiagnostic{explanation + " Current local facts, not the original refusal or provider state.", action}
}
