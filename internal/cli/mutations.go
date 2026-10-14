package cli

import (
	"context"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
)

type mutationInvocation struct {
	operation string
	key       string
	id        string
	create    client.CreateJobRequest
	edit      client.EditJobRequest
	send      client.SendMessageRequest
}

func isMutation(command, operation string) bool {
	switch command + " " + operation {
	case "jobs create", "jobs edit", "inbox send", "inbox seen", "inbox ack":
		return true
	}
	return false
}

// Parse the complete request before loading a profile or credential. Explicit
// keys make retries recoverable across separate CLI processes without a global
// retry identity or silently regenerated key.
func parseMutation(command, operation string, args []string) (*mutationInvocation, bool) {
	call := &mutationInvocation{operation: command + " " + operation}
	allowed := map[string]bool{"--key": true}
	switch call.operation {
	case "jobs create", "jobs edit":
		for _, flag := range []string{"--title", "--brief", "--worker", "--status"} {
			allowed[flag] = true
		}
		if operation == "edit" {
			allowed["--expect-revision"] = true
		}
	case "inbox send":
		for _, flag := range []string{"--to", "--body", "--reply-to"} {
			allowed[flag] = true
		}
	}
	values := make(map[string]string)
	var positions []string
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
		if _, duplicate := values[flag]; duplicate {
			return nil, false
		}
		if !assigned {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "--") {
				return nil, false
			}
			value = args[i]
		}
		if !utf8.ValidString(value) || value == "" && flag != "--brief" && !(call.operation == "jobs edit" && flag == "--worker") {
			return nil, false
		}
		values[flag] = value
	}
	call.key = values["--key"]
	if !validMutationKey(call.key) {
		return nil, false
	}
	wantsID := call.operation == "jobs edit" || call.operation == "inbox seen" || call.operation == "inbox ack"
	if wantsID {
		if len(positions) != 1 || !validResourceID(positions[0]) {
			return nil, false
		}
		call.id = positions[0]
	} else if len(positions) != 0 {
		return nil, false
	}
	if title, present := values["--title"]; present && (strings.TrimSpace(title) == "" || len(strings.TrimSpace(title)) > 240) {
		return nil, false
	}
	if len(values["--brief"]) > 32<<10 {
		return nil, false
	}
	if worker := values["--worker"]; worker != "" && !validResourceID(worker) {
		return nil, false
	}
	if status, present := values["--status"]; present && !validStatus(status) {
		return nil, false
	}
	switch call.operation {
	case "jobs create":
		if _, present := values["--title"]; !present {
			return nil, false
		}
		call.create = client.CreateJobRequest{Title: values["--title"], Brief: values["--brief"], WorkerID: values["--worker"], Status: values["--status"]}
	case "jobs edit":
		revision, err := strconv.ParseInt(values["--expect-revision"], 10, 64)
		if err != nil || revision < 1 || revision == (1<<63)-1 {
			return nil, false
		}
		call.edit.ExpectedRevision = revision
		changed := false
		for flag, destination := range map[string]**string{"--title": &call.edit.Title, "--brief": &call.edit.Brief, "--worker": &call.edit.WorkerID, "--status": &call.edit.Status} {
			if value, present := values[flag]; present {
				copy := value
				*destination = &copy
				changed = true
			}
		}
		if !changed {
			return nil, false
		}
	case "inbox send":
		if !validResourceID(values["--to"]) || strings.TrimSpace(values["--body"]) == "" || len(values["--body"]) > 16<<10 {
			return nil, false
		}
		if reply, present := values["--reply-to"]; present && !validResourceID(reply) {
			return nil, false
		}
		call.send = client.SendMessageRequest{RecipientWorkerID: values["--to"], Body: values["--body"], ReplyToMessageID: values["--reply-to"]}
	}
	return call, true
}

func validMutationKey(key string) bool {
	if len(key) == 0 || len(key) > 128 || key != strings.TrimSpace(key) {
		return false
	}
	for _, b := range []byte(key) {
		if b < 32 || b > 126 {
			return false
		}
	}
	return true
}

func executeMutation(ctx context.Context, service *client.Client, call *mutationInvocation) (any, error) {
	switch call.operation {
	case "jobs create":
		return service.CreateJob(ctx, call.key, call.create)
	case "jobs edit":
		return service.EditJob(ctx, call.key, call.id, call.edit)
	case "inbox send":
		return service.SendMessage(ctx, call.key, call.send)
	case "inbox seen":
		return service.MarkSeen(ctx, call.key, call.id)
	default:
		return service.Acknowledge(ctx, call.key, call.id)
	}
}
