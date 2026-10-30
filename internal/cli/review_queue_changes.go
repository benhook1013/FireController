package cli

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/client"
)

type reviewQueueChangeInvocation struct {
	action, key string
	input       client.ReviewQueueChange
}

func parseReviewQueueChange(call invocation, args []string) (invocation, bool) {
	if len(args) < 1 {
		return call, false
	}
	parseNumber := func(s string) (int64, bool) {
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil && n > 0 && strconv.FormatInt(n, 10) == s
	}
	number, ok := parseNumber(args[0])
	if !ok {
		return call, false
	}
	in := &reviewQueueChangeInvocation{action: strings.TrimPrefix(call.subcommand, "queue-"), input: client.ReviewQueueChange{PullNumber: number}}
	seen := map[string]bool{}
	for i := 1; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
		if seen[flag] {
			return call, false
		}
		seen[flag] = true
		if flag == "--first" || flag == "--last" {
			if assigned || in.input.Position != "" {
				return call, false
			}
			in.input.Position = strings.TrimPrefix(flag, "--")
			continue
		}
		if flag != "--before" && flag != "--after" && flag != "--expect-config" && flag != "--key" && flag != "--reason" {
			return call, false
		}
		if !assigned {
			i++
			if i >= len(args) {
				return call, false
			}
			value = args[i]
		}
		if flag == "--reason" {
			in.input.Reason = value
			continue
		}
		if flag == "--key" {
			in.key = value
			continue
		}
		n, ok := parseNumber(value)
		if !ok {
			return call, false
		}
		switch flag {
		case "--before":
			in.input.Before = &n
		case "--after":
			in.input.After = &n
		case "--expect-config":
			in.input.ExpectedConfigurationRevision = n
		}
	}
	if !client.ValidMutationKey(in.key) || client.ValidateReviewQueueChange(in.action, in.input) != nil {
		return call, false
	}
	call.reviewQueueChange = in
	return call, true
}
func executeReviewQueueChange(ctx, parent context.Context, c *client.Client, in *reviewQueueChangeInvocation, stdout, stderr io.Writer) int {
	out, err := c.ChangeReviewQueue(ctx, in.action, in.key, in.input)
	if err != nil {
		return reportClientError(stderr, err, parent)
	}
	body, err := json.Marshal(out)
	if err == nil {
		body = append(body, '\n')
		var n int
		n, err = stdout.Write(body)
		if err == nil && n != len(body) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The queue-change receipt could not be written.", Action: "Retry the exact same queue action, arguments, key and binding; do not invent a replacement key.", OutcomeUnknown: true})
	}
	return ExitSuccess
}
