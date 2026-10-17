package cli

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/client"
)

type reviewRunInvocation struct {
	read        bool
	key, id     string
	request     client.ReviewRunRequest
	waitSeconds int
}

func parseReviewRun(call invocation, args []string) (invocation, bool) {
	in := &reviewRunInvocation{read: call.subcommand == "run-read", waitSeconds: 2}
	allowed := []string{"--channel", "--key", "--expect-pr", "--expected-configuration-revision", "--wait-seconds"}
	if in.read {
		in.waitSeconds = 0
		allowed = []string{"--selection-id", "--wait-seconds"}
	}
	values, ok := reviewRunFlags(args, allowed)
	if !ok {
		return call, false
	}
	if raw, present := values["--wait-seconds"]; present {
		n, err := strconv.Atoi(raw)
		if err != nil || strconv.Itoa(n) != raw || client.ValidateReviewOperationWait(n) != nil {
			return call, false
		}
		in.waitSeconds = n
	}
	if in.read {
		in.id = values["--selection-id"]
		if client.ValidateReviewRunSelectionID(in.id) != nil {
			return call, false
		}
	} else {
		in.key = values["--key"]
		in.request.Channel = values["--channel"]
		if !validMutationKey(in.key) {
			return call, false
		}
		for _, flag := range []string{"--expect-pr", "--expected-configuration-revision"} {
			if raw, present := values[flag]; present {
				n, err := strconv.ParseInt(raw, 10, 64)
				if err != nil || n < 1 || strconv.FormatInt(n, 10) != raw {
					return call, false
				}
				if flag == "--expect-pr" {
					in.request.ExpectedPullNumber = &n
				} else {
					in.request.ExpectedConfigurationRevision = &n
				}
			}
		}
		if client.ValidateReviewRunRequest(in.request) != nil {
			return call, false
		}
	}
	call.reviewRun = in
	return call, true
}
func executeReviewRun(ctx, parent context.Context, c *client.Client, in *reviewRunInvocation, stdout, stderr io.Writer) int {
	if in.read {
		value, err := c.WaitReviewSelection(ctx, in.id, in.waitSeconds)
		if err != nil {
			return reportClientError(stderr, err, parent)
		}
		return emit(stdout, stderr, value)
	}
	value, err := c.RunReview(ctx, in.key, in.request, in.waitSeconds)
	if err != nil {
		return reportClientError(stderr, err, parent)
	}
	raw, err := json.Marshal(value)
	if err == nil {
		raw = append(raw, '\n')
		var n int
		n, err = stdout.Write(raw)
		if err == nil && n != len(raw) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The selection result could not be written.", Action: "Retry the exact same key, selection arguments and bound instance; do not invent a replacement key.", OutcomeUnknown: true})
	}
	return ExitSuccess
}

func reviewRunFlags(args, flags []string) (map[string]string, bool) {
	allowed := make(map[string]bool, len(flags))
	for _, flag := range flags {
		allowed[flag] = true
	}
	values := make(map[string]string, len(flags))
	for i := 0; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
		if !allowed[flag] {
			return nil, false
		}
		if _, duplicate := values[flag]; duplicate {
			return nil, false
		}
		if !assigned {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "--") {
				return nil, false
			}
			value = args[i]
		}
		if value == "" {
			return nil, false
		}
		values[flag] = value
	}
	return values, true
}
