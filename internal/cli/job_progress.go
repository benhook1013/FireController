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
		for _, flag := range []string{"--title", "--brief", "--brief-file", "--worker", "--status", "--progress", "--progress-file", "--next", "--next-file", "--blocker", "--blocker-file", "--evidence"} {
			allowed[flag] = true
		}
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
		files := map[string]int{"--brief": 32 << 10, "--progress": 8 << 10, "--next": 8 << 10, "--blocker": 4 << 10}
		if !prepareTextFiles(values, files) {
			return nil, false
		}
		call.patch = client.UpdateJobRequest{ExpectedRevision: revision}
		for flag, dst := range map[string]**string{"--title": &call.patch.Title, "--brief": &call.patch.Brief, "--worker": &call.patch.WorkerID, "--status": &call.patch.Status} {
			if v, ok := values[flag]; ok {
				value := v
				*dst = &value
			}
		}
		progress := &client.ProgressPatch{}
		for flag, dst := range map[string]**string{"--progress": &progress.Summary, "--next": &progress.Next, "--blocker": &progress.Blocker} {
			if v, ok := values[flag]; ok {
				value := v
				*dst = &value
			}
		}
		if _, ok := values["--evidence"]; ok {
			if len(evidence) == 1 && evidence[0] == "" {
				evidence = []string{}
			} else {
				for _, ref := range evidence {
					if ref == "" {
						return nil, false
					}
				}
			}
			progress.Evidence = &evidence
		}
		if progress.Summary != nil || progress.Next != nil || progress.Blocker != nil || progress.Evidence != nil {
			call.patch.Progress = progress
		}
		if client.ValidateJobPatch(call.patch) != nil {
			return nil, false
		}
		if ok, failure := loadTextFiles(values, files); !ok {
			call.argumentDiagnostic = failure
			return call, false
		}
		for flag, dst := range map[string]**string{"--brief": &call.patch.Brief, "--progress": &progress.Summary, "--next": &progress.Next, "--blocker": &progress.Blocker} {
			if _, fromFile := values[flag+"-file"]; fromFile {
				value := values[flag]
				*dst = &value
			}
		}
		return call, client.ValidateJobPatch(call.patch) == nil
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
