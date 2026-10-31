package cli

import (
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/client"
)

func parseChecklistItem(args []string) (*mutationInvocation, bool) {
	call := &mutationInvocation{operation: "jobs checklist-item"}
	values := make(map[string]string)
	var positions []string
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			positions = append(positions, args[i])
			continue
		}
		flag, value, assigned := strings.Cut(args[i], "=")
		if flag != "--key" && flag != "--done" && flag != "--expect-checklist-revision" {
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
		values[flag] = value
	}
	if len(positions) != 2 || !validResourceID(positions[0]) || !client.ValidChecklistItemID(positions[1]) || !validMutationKey(values["--key"]) || values["--done"] != "true" && values["--done"] != "false" {
		return nil, false
	}
	revision, err := strconv.ParseInt(values["--expect-checklist-revision"], 10, 64)
	if err != nil {
		return nil, false
	}
	call.id, call.itemID, call.key = positions[0], positions[1], values["--key"]
	call.checklistItem = client.UpdateChecklistItemRequest{ExpectedRevision: revision, Done: values["--done"] == "true"}
	return call, client.ValidateChecklistItemRequest(call.checklistItem) == nil
}
