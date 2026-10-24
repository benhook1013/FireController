package cli

import (
	"context"
	"github.com/benhook1013/FireController/internal/client"
	"strconv"
	"strings"
)

type reviewOperationStatusInvocation struct {
	ID          string
	WaitSeconds int
	Diagnostics bool
}

func parseReviewOperationStatus(args []string) (*reviewOperationStatusInvocation, bool) {
	value := &reviewOperationStatusInvocation{}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			if value.ID != "" {
				return nil, false
			}
			value.ID = arg
			continue
		}
		flag, raw, assigned := strings.Cut(arg, "=")
		if seen[flag] {
			return nil, false
		}
		seen[flag] = true
		switch flag {
		case "--diagnostics":
			if assigned {
				return nil, false
			}
			value.Diagnostics = true
		case "--wait-seconds":
			if !assigned {
				i++
				if i >= len(args) {
					return nil, false
				}
				raw = args[i]
			}
			n, err := strconv.Atoi(raw)
			if err != nil || strconv.Itoa(n) != raw || client.ValidateReviewOperationWait(n) != nil {
				return nil, false
			}
			value.WaitSeconds = n
		default:
			return nil, false
		}
	}
	return value, client.ValidateReviewOperationID(value.ID) == nil
}
func executeReviewOperationStatus(ctx context.Context, service *client.Client, value *reviewOperationStatusInvocation) (any, error) {
	if value.Diagnostics {
		return service.WaitReviewOperation(ctx, value.ID, value.WaitSeconds)
	}
	status, err := service.WaitReviewOperationStatus(ctx, value.ID, value.WaitSeconds)
	return client.AgentReviewOperationStatus(status), err
}
