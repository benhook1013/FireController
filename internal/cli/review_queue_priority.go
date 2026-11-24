package cli

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/client"
)

type reviewQueuePriorityInvocation struct {
	key    string
	change client.ReviewQueuePriorityChange
}

func parseReviewQueuePriority(call invocation, args []string) (invocation, bool) {
	in := &reviewQueuePriorityInvocation{}
	if len(args) == 0 {
		call.reviewQueuePriority = in
		return call, true
	}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
		if seen[flag] {
			return call, false
		}
		seen[flag] = true
		if !assigned {
			i++
			if i >= len(args) {
				return call, false
			}
			value = args[i]
		}
		switch flag {
		case "--key":
			in.key = value
		case "--expect-revision":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 || strconv.FormatInt(n, 10) != value {
				return call, false
			}
			in.change.ExpectedRevision = n
		case "--priority":
			if value != "true" && value != "false" {
				return call, false
			}
			in.change.Priority = value == "true"
		case "--pulls":
			for _, part := range strings.Split(value, ",") {
				n, err := strconv.ParseInt(part, 10, 32)
				if err != nil || n <= 0 || strconv.FormatInt(n, 10) != part || slices.Contains(in.change.PullNumbers, n) {
					return call, false
				}
				in.change.PullNumbers = append(in.change.PullNumbers, n)
			}
		default:
			return call, false
		}
	}
	if len(seen) != 4 || len(in.change.PullNumbers) == 0 || len(in.change.PullNumbers) > 100 || !client.ValidMutationKey(in.key) {
		return call, false
	}
	slices.Sort(in.change.PullNumbers)
	call.reviewQueuePriority = in
	return call, true
}

func executeReviewQueuePriority(ctx, parent context.Context, c *client.Client, in *reviewQueuePriorityInvocation, stdout, stderr io.Writer) int {
	var out client.ReviewQueuePriority
	var err error
	if in.key == "" {
		out, err = c.ReadReviewQueuePriority(ctx)
	} else {
		out, err = c.SetReviewQueuePriority(ctx, in.key, in.change)
	}
	if err != nil {
		return reportClientError(stderr, err, parent)
	}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "Queue priority could not be written to stdout.", Action: "For a write, retry the exact request and key.", OutcomeUnknown: in.key != ""})
	}
	return ExitSuccess
}
