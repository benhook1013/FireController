package cli

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
)

func parseCheckpoint(args []string) (*mutationInvocation, bool) {
	call := &mutationInvocation{operation: "jobs checkpoint"}
	values := make(map[string]string)
	var positions []string
	var evidence []string
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if !strings.HasPrefix(argument, "-") {
			positions = append(positions, argument)
			continue
		}
		flag, value, assigned := strings.Cut(argument, "=")
		if flag != "--key" && flag != "--expect-revision" && flag != "--done" && flag != "--next" && flag != "--blocker" && flag != "--evidence" {
			return nil, false
		}
		if _, exists := values[flag]; exists && flag != "--evidence" {
			return nil, false
		}
		if !assigned {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "--") {
				return nil, false
			}
			value = args[i]
		}
		if !utf8.ValidString(value) {
			return nil, false
		}
		if flag == "--evidence" {
			evidence = append(evidence, value)
			if len(evidence) > 10 {
				return nil, false
			}
		}
		values[flag] = value
	}
	if len(positions) != 1 || !validResourceID(positions[0]) || !validMutationKey(values["--key"]) {
		return nil, false
	}
	revision, err := strconv.ParseInt(values["--expect-revision"], 10, 64)
	if err != nil {
		return nil, false
	}
	request := client.AppendCheckpointRequest{ExpectedJobRevision: revision, Done: values["--done"], Next: values["--next"], Blocker: values["--blocker"], Evidence: evidence}
	if client.ValidateCheckpointRequest(request) != nil {
		return nil, false
	}
	call.id, call.key, call.checkpoint = positions[0], values["--key"], request
	return call, true
}
