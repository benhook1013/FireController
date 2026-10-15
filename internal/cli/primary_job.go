package cli

import (
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/client"
)

func parsePrimaryJob(operation string, args []string) (*mutationInvocation, bool) {
	call := &mutationInvocation{operation: "jobs " + operation}
	values := map[string]string{}
	var positions []string
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if !strings.HasPrefix(argument, "-") {
			positions = append(positions, argument)
			continue
		}
		flag, value, assigned := strings.Cut(argument, "=")
		if flag != "--key" && flag != "--expect-revision" {
			return nil, false
		}
		if _, duplicate := values[flag]; duplicate {
			return nil, false
		}
		if !assigned {
			i++
			if i >= len(args) {
				return nil, false
			}
			value = args[i]
		}
		values[flag] = value
	}
	expected := 0
	if operation == "primary-set" {
		expected = 1
	}
	if len(positions) != expected || !validMutationKey(values["--key"]) {
		return nil, false
	}
	revision, err := strconv.ParseInt(values["--expect-revision"], 10, 64)
	if err != nil {
		return nil, false
	}
	call.primary = client.ReplacePrimaryJobRequest{ExpectedRevision: revision}
	if expected == 1 {
		call.primary.JobID = positions[0]
		if !validResourceID(call.primary.JobID) {
			return nil, false
		}
	}
	call.key = values["--key"]
	return call, client.ValidatePrimaryJobRequest(call.primary) == nil
}
