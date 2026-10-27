package cli

import (
	"strings"

	"github.com/benhook1013/FireController/internal/client"
)

// Parsing and private-file admission happen before a selected connection loads.
func parseReviewBatch(operation string, args []string) (*mutationInvocation, bool) {
	if operation != "decide-batch" && operation != "fix-batch" {
		return nil, false
	}
	call := &mutationInvocation{operation: "reviews " + operation}
	values := map[string]string{}
	var positions []string
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			positions = append(positions, args[i])
			continue
		}
		flag, value, assigned := strings.Cut(args[i], "=")
		if flag != "--file" && flag != "--key" {
			return nil, false
		}
		if _, ok := values[flag]; ok {
			return nil, false
		}
		if !assigned {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "--") {
				return nil, false
			}
			value = args[i]
		}
		values[flag] = value
	}
	if len(positions) != 1 || client.ValidateReviewEvidenceID(positions[0]) != nil || !validMutationKey(values["--key"]) || values["--file"] == "" {
		return nil, false
	}
	data, ok, diagnostic := readPrivateTextFileArgument(values["--file"], client.ReviewBatchMaxBytes, "--file")
	if !ok {
		call.argumentDiagnostic = diagnostic
		return call, false
	}
	call.id, call.key = positions[0], values["--key"]
	var err error
	if operation == "decide-batch" {
		call.reviewDecisionBatch, err = client.DecodeReviewDecisionBatchRequest([]byte(data))
	} else {
		call.reviewFixBatch, err = client.DecodeReviewFixBatchRequest([]byte(data))
	}
	return call, err == nil
}
