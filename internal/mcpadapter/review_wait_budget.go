package mcpadapter

import (
	"context"
	"time"
)

// Setup shares the tool deadline with the service call. Wait is an upper bound,
// so leave the existing five-second response budget after authentication.
func fitReviewWait(ctx context.Context, name string, args arguments) arguments {
	switch name {
	case "review_start", "review_selection_read", "review_request", "review_operation_read", "review_source_refresh", "review_checkpoint_publish", "review_checkpoint_recover":
	default:
		return args
	}
	deadline, ok := ctx.Deadline()
	if !ok || args.reviewSourceWait <= 0 {
		return args
	}
	args.reviewSourceWait = reviewWaitWithinBudget(args.reviewSourceWait, time.Until(deadline))
	if args.operationStatus != nil {
		input := *args.operationStatus
		input.WaitSeconds = args.reviewSourceWait
		args.operationStatus = &input
	}
	return args
}

func reviewWaitWithinBudget(requested int, remaining time.Duration) int {
	available := max(0, remaining-5*time.Second) / time.Second
	return min(requested, int(available))
}
