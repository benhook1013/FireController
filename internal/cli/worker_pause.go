package cli

import (
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/client"
)

func parseWorkerPause(operation string, args []string) (*mutationInvocation, bool) {
	call := &mutationInvocation{operation: "workers " + operation}
	values := map[string]string{}
	for i := 0; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
		if flag != "--key" && flag != "--expect-revision" && !(operation == "pause" && flag == "--reason") {
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
	if !validMutationKey(values["--key"]) {
		return nil, false
	}
	revision, err := strconv.ParseInt(values["--expect-revision"], 10, 64)
	if err != nil {
		return nil, false
	}
	reason, present := values["--reason"]
	if operation == "pause" && !present {
		return nil, false
	}
	call.pause = client.ReplaceWorkerPauseRequest{ExpectedRevision: revision, Paused: operation == "pause", Reason: reason}
	call.key = values["--key"]
	return call, client.ValidateWorkerPauseRequest(call.pause) == nil
}
