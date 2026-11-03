package cli

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
)

func parseJobSummary(args []string) (*mutationInvocation, bool) {
	call := &mutationInvocation{operation: "jobs summary-set"}
	allowed := map[string]bool{"--key": true, "--expect-revision": true, "--expect-summary-revision": true, "--title": true, "--body": true, "--body-file": true}
	values := map[string]string{}
	var positions []string
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			positions = append(positions, args[i])
			continue
		}
		flag, value, assigned := strings.Cut(args[i], "=")
		if !allowed[flag] {
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
		if !utf8.ValidString(value) {
			return nil, false
		}
		values[flag] = value
	}
	if len(positions) != 1 || !validResourceID(positions[0]) || !validMutationKey(values["--key"]) || len(values) != 5 {
		return nil, false
	}
	job, err := strconv.ParseInt(values["--expect-revision"], 10, 64)
	if err != nil {
		return nil, false
	}
	summary, err := strconv.ParseInt(values["--expect-summary-revision"], 10, 64)
	if err != nil {
		return nil, false
	}
	body, literal := values["--body"]
	file, fromFile := values["--body-file"]
	if literal == fromFile {
		return nil, false
	}
	if fromFile {
		var ok bool
		body, ok = readNoteBodyFile(file)
		if !ok {
			return nil, false
		}
	}
	call.id = positions[0]
	call.key = values["--key"]
	call.summary = client.SetJobSummaryRequest{ExpectedJobRevision: job, ExpectedRevision: summary, Title: values["--title"], Body: body}
	return call, client.ValidateJobSummaryRequest(call.summary) == nil
}
