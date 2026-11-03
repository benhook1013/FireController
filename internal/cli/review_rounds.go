package cli

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/client"
)

type reviewRoundsInvocation struct {
	operation    string
	pull         int64
	channel, key string
	input        client.ConfigureReviewRoundControlRequest
}

func parseReviewRounds(args []string) (*reviewRoundsInvocation, bool) {
	if len(args) == 0 {
		return nil, false
	}
	in := &reviewRoundsInvocation{operation: args[0]}
	allowed := map[string]bool{"--pr": true, "--channel": true}
	switch in.operation {
	case "read":
	case "grant":
		allowed["--expected-revision"], allowed["--key"], allowed["--min"], allowed["--max"], allowed["--exact"] = true, true, true, true, true
	case "close":
		allowed["--expected-revision"], allowed["--key"] = true, true
	default:
		return nil, false
	}
	values := map[string]string{}
	for i := 1; i < len(args); i++ {
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
		if value == "" {
			return nil, false
		}
		values[flag] = value
	}
	number := func(flag string, max int64) (int64, bool) {
		s, ok := values[flag]
		if !ok {
			return 0, false
		}
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil && n >= 0 && n <= max && strconv.FormatInt(n, 10) == s
	}
	var ok bool
	in.pull, ok = number("--pr", 2147483647)
	if !ok || in.pull == 0 {
		return nil, false
	}
	in.channel = values["--channel"]
	if in.channel != "cli" && in.channel != "hosted" {
		return nil, false
	}
	if in.operation == "read" {
		return in, true
	}
	in.key = values["--key"]
	if !validMutationKey(in.key) {
		return nil, false
	}
	revision, ok := number("--expected-revision", math.MaxInt64-1)
	if !ok {
		return nil, false
	}
	in.input.ExpectedRevision = &revision
	in.input.Action = in.operation
	if in.operation == "grant" {
		var minimum, maximum int64
		if _, exact := values["--exact"]; exact {
			if _, has := values["--min"]; has {
				return nil, false
			}
			if _, has := values["--max"]; has {
				return nil, false
			}
			maximum, ok = number("--exact", 1000)
			minimum = maximum
		} else {
			maximum, ok = number("--max", 1000)
			if _, has := values["--min"]; has {
				var valid bool
				minimum, valid = number("--min", 1000)
				if !valid {
					return nil, false
				}
			}
		}
		if !ok || maximum < 1 || minimum > maximum {
			return nil, false
		}
		in.input.Minimum = &minimum
		in.input.Maximum = &maximum
	}
	return in, client.ValidateReviewRoundControlRequest(in.input) == nil
}

func executeReviewRounds(ctx, parent context.Context, c *client.Client, in *reviewRoundsInvocation, stdout, stderr io.Writer) int {
	if in.operation == "read" {
		value, err := c.ReviewRoundControl(ctx, in.pull, in.channel)
		if err != nil {
			return reportClientError(stderr, err, parent)
		}
		return emit(stdout, stderr, value)
	}
	value, err := c.ConfigureReviewRoundControl(ctx, in.pull, in.channel, in.key, in.input)
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
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The round-control receipt could not be written.", Action: "Retry the exact same key, round-control arguments and bound instance; do not invent a replacement key.", OutcomeUnknown: true})
	}
	return ExitSuccess
}
