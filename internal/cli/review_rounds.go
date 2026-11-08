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
	helpCommand  string
	pull         int64
	channel, key string
	input        client.ConfigureReviewRoundControlRequest
}

func parseReviewRounds(args []string) (*reviewRoundsInvocation, bool) {
	if len(args) == 0 {
		return nil, false
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "help") {
		return &reviewRoundsInvocation{operation: "help"}, true
	}
	if len(args) == 2 && args[1] == "--help" && (args[0] == "read" || args[0] == "grant" || args[0] == "close" || args[0] == "taper") {
		return &reviewRoundsInvocation{operation: "help", helpCommand: args[0]}, true
	}
	in := &reviewRoundsInvocation{operation: args[0]}
	allowed := map[string]bool{"--pr": true, "--channel": true}
	switch in.operation {
	case "read":
	case "grant":
		allowed["--expect-revision"], allowed["--key"], allowed["--min"], allowed["--max"], allowed["--exact"] = true, true, true, true, true
	case "close":
		allowed["--expect-revision"], allowed["--key"] = true, true
	case "taper":
		allowed["--expect-revision"], allowed["--key"], allowed["--taper"] = true, true, true
	default:
		return nil, false
	}
	values := map[string]string{}
	for i := 1; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
		if flag == "--expected-revision" {
			flag = "--expect-revision"
		}
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
	revision, ok := number("--expect-revision", math.MaxInt64-1)
	if !ok {
		return nil, false
	}
	in.input.ExpectedRevision = &revision
	in.input.Action = in.operation
	if in.operation == "taper" {
		taper, valid := number("--taper", 1000)
		if !valid || taper < 1 {
			return nil, false
		}
		in.input.Taper = &taper
	}
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

// Round help is local and must run before resolving any profile or credential.
func emitReviewRoundsHelp(in *reviewRoundsInvocation, stdout, stderr io.Writer) int {
	usage := "fc reviews rounds read|grant|close|taper --help"
	required, optional := []string{}, []string{}
	semantics := []string{"Reads and settings are provider-free. Settings never request a review.", "Use an explicit original key and current revision for mutations; preserve both after an uncertain result."}
	if in.helpCommand != "" {
		usage = "fc reviews rounds " + in.helpCommand + " --pr NUMBER --channel cli|hosted"
		required = append(required, "--pr NUMBER", "--channel cli|hosted")
		if in.helpCommand != "read" {
			usage += " --expect-revision N --key KEY"
			required = append(required, "--expect-revision N", "--key KEY")
			optional = append(optional, "--expected-revision N (compatible alias; do not combine revision flags)")
		}
		switch in.helpCommand {
		case "read":
			semantics = append(semantics, "Returns configured bounds, completed count, dry streak, taper, closure and revision.")
		case "grant":
			usage += " (--max N [--min N] | --exact N)"
			required = append(required, "--max N or --exact N")
			optional = append(optional, "--min N (default zero; incompatible with --exact)")
		case "taper":
			usage += " --taper N"
			required = append(required, "--taper N (1-1000)")
			semantics = append(semantics, "Changes only the clean-streak threshold, preserving allowance and closure.")
		case "close":
			semantics = append(semantics, "Stops new dispatch without cancelling possibly posted work; no allocation fields.")
		}
	}
	return emit(stdout, stderr, struct {
		Usage     string   `json:"usage"`
		Required  []string `json:"required"`
		Optional  []string `json:"optional"`
		Semantics []string `json:"semantics"`
	}{usage, required, optional, semantics})
}
