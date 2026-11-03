package cli

import (
	"context"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/client"
)

type reviewSchedulingInvocation struct {
	action string
	pull   int64
	key    string
	config client.ConfigureReviewSchedulingRequest
	pause  client.ConfigureReviewPullSchedulingRequest
}

func parseReviewScheduling(args []string) (*reviewSchedulingInvocation, bool) {
	in := &reviewSchedulingInvocation{action: "read"}
	if len(args) > 0 && !strings.HasPrefix(args[0], "--") {
		in.action, args = args[0], args[1:]
	}
	allowed := map[string]bool{"--pr": true}
	switch in.action {
	case "read":
	case "configure":
		allowed = map[string]bool{"--automatic-hosted": true, "--expect-revision": true, "--key": true}
	case "pause", "resume":
		allowed["--expect-revision"], allowed["--key"] = true, true
		if in.action == "pause" {
			allowed["--reason"] = true
		}
	default:
		return nil, false
	}
	values := map[string]string{}
	for i := 0; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
		if !allowed[flag] {
			return nil, false
		}
		if _, exists := values[flag]; exists {
			return nil, false
		}
		if !assigned {
			i++
			if i >= len(args) {
				return nil, false
			}
			value = args[i]
		}
		if value == "" && flag != "--reason" {
			return nil, false
		}
		values[flag] = value
	}
	if value, exists := values["--pr"]; exists {
		n, err := strconv.ParseInt(value, 10, 32)
		if err != nil || n < 1 || strconv.FormatInt(n, 10) != value {
			return nil, false
		}
		in.pull = n
	}
	if in.action == "read" {
		return in, true
	}
	if !validMutationKey(values["--key"]) {
		return nil, false
	}
	in.key = values["--key"]
	revision, err := strconv.ParseInt(values["--expect-revision"], 10, 64)
	if err != nil || strconv.FormatInt(revision, 10) != values["--expect-revision"] {
		return nil, false
	}
	if in.action == "configure" {
		value := values["--automatic-hosted"]
		if value != "true" && value != "false" {
			return nil, false
		}
		in.config = client.ConfigureReviewSchedulingRequest{ExpectedRevision: revision, AutomaticHosted: value == "true"}
		return in, client.ValidateConfigureReviewScheduling(in.config) == nil
	}
	in.pause = client.ConfigureReviewPullSchedulingRequest{ExpectedRevision: revision, Paused: in.action == "pause", Reason: values["--reason"]}
	return in, client.ValidateConfigureReviewPullScheduling(in.pull, in.pause) == nil
}

func executeReviewScheduling(ctx context.Context, service *client.Client, in *reviewSchedulingInvocation) (any, error) {
	switch in.action {
	case "configure":
		return service.ConfigureReviewScheduling(ctx, in.key, in.config)
	case "pause", "resume":
		return service.ConfigureReviewPullScheduling(ctx, in.key, in.pull, in.pause)
	default:
		if in.pull != 0 {
			return service.ReviewPullScheduling(ctx, in.pull)
		}
		return service.ReviewScheduling(ctx)
	}
}
