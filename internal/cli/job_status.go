package cli

import (
	"context"

	"github.com/benhook1013/FireController/internal/client"
)

// parseJobStatus receives only arguments following "jobs status".
func parseJobStatus(args []string) (invocation, bool) {
	call := invocation{command: "jobs", subcommand: "status"}
	if len(args) != 1 || !validResourceID(args[0]) {
		return call, false
	}
	call.position = append([]string(nil), args...)
	return call, true
}

func runJobStatus(ctx context.Context, service *client.Client, id string) (any, error) {
	return service.JobStatus(ctx, id)
}
