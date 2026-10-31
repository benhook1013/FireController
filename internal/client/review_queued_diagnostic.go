package client

func recognizedQueuedCause(value string) string {
	switch value {
	case "operation_paused", "project_paused", "cooldown", "queue_order", "reservation_busy", "awaiting_scheduler":
		return value
	}
	return ""
}
func queuedOperationDiagnostic(cause string) *ReviewOperationDiagnostic {
	var explanation string
	switch cause {
	case "operation_paused":
		explanation = "This queued operation is explicitly paused."
	case "project_paused":
		explanation = "Project execution is paused."
	case "cooldown":
		explanation = "This queued operation’s cooldown deadline is still in the future."
	case "queue_order":
		explanation = "An earlier applicable Hosted request in this project’s PR order is queued, possibly on a future timer."
	case "reservation_busy":
		explanation = "A matching provider-account or target reservation is occupied. A due timer does not release it."
	case "awaiting_scheduler":
		explanation = "Queued for scheduler assessment; worker activity and provider availability are not established."
	default:
		return nil
	}
	return &ReviewOperationDiagnostic{explanation + " Current local facts, not the original refusal or provider state.", "Keep the original request; do not submit a replacement. A due timer is eligibility, not a start ETA. Normal admission and provider checks still apply."}
}
