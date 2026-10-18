package cli

import (
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
)

func parseJobProgress(operation string, args []string) (*mutationInvocation, bool) {
	call := &mutationInvocation{operation: "jobs " + operation}
	allowed := map[string]bool{"--key": true, "--expect-revision": true}
	if operation == "update" {
		allowed["--body"], allowed["--evidence"] = true, true
	} else {
		allowed["--expect-checklist-revision"], allowed["--items"], allowed["--items-file"] = true, true, true
	}
	values := make(map[string]string)
	var positions, evidence []string
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if !strings.HasPrefix(argument, "-") {
			positions = append(positions, argument)
			continue
		}
		flag, value, assigned := strings.Cut(argument, "=")
		if !allowed[flag] {
			return nil, false
		}
		if _, duplicate := values[flag]; duplicate && flag != "--evidence" {
			return nil, false
		}
		if !assigned {
			i++
			if i >= len(args) {
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
	if err != nil || revision < 1 {
		return nil, false
	}
	call.id, call.key = positions[0], values["--key"]
	if operation == "update" {
		call.update = client.AppendUpdateRequest{ExpectedJobRevision: revision, Body: values["--body"], Evidence: evidence}
		return call, client.ValidateJobUpdateRequest(call.update) == nil
	}
	checklistRevision, err := strconv.ParseInt(values["--expect-checklist-revision"], 10, 64)
	if err != nil || checklistRevision < 0 || checklistRevision == 1<<63-1 {
		return nil, false
	}
	items, literal := values["--items"]
	file, fromFile := values["--items-file"]
	if literal == fromFile {
		return nil, false
	}
	var data []byte
	if literal {
		data = []byte(items)
	} else {
		var ok bool
		data, ok = readChecklistFile(file)
		if !ok {
			return nil, false
		}
	}
	parsed, err := client.DecodeJobChecklistItems(data)
	if err != nil {
		return nil, false
	}
	call.checklist = client.ReplaceChecklistRequest{ExpectedJobRevision: revision, ExpectedRevision: checklistRevision, Items: parsed}
	return call, client.ValidateJobChecklistRequest(call.checklist) == nil
}

// Inspect before opening to avoid accidental FIFO/device waits. As with local
// profiles this does not claim protection against hostile filesystem races.
func readChecklistFile(path string) ([]byte, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 256<<10 {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(f, (256<<10)+1))
	return data, err == nil && len(data) <= 256<<10
}
