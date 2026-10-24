package cli

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
)

type mutationInvocation struct {
	argumentDiagnostic       *diagnostic
	summary                  client.SetJobSummaryRequest
	reviewCheckpoint         client.ReviewCheckpointRequest
	reviewCheckpointRecovery client.ReviewCheckpointRecovery
	operation                string
	key                      string
	id                       string
	itemID                   string
	checklistItem            client.UpdateChecklistItemRequest
	create                   client.CreateJobRequest
	send                     client.SendMessageRequest
	patch                    client.UpdateJobRequest
	checklist                client.ReplaceChecklistRequest
	primary                  client.ReplacePrimaryJobRequest
	pause                    client.ReplaceWorkerPauseRequest
	reviewRoute              client.ReviewRouteRequest
	reviewRouteDecision      client.ReviewRouteDecisionRequest
	reviewRouteFix           client.ReviewRouteFixRequest
	reviewDecision           client.ReviewDecisionRequest
	reviewFix                client.ReviewFixRequest
	reviewDecisionBatch      client.ReviewDecisionBatchRequest
	reviewFixBatch           client.ReviewFixBatchRequest
	reviewFinalize           client.ReviewFinalizeRequest
	reviewRequest            client.ReviewRequest
	reviewSource             client.ReviewSourceRequest
	reviewSourceWait         int
}

func isMutation(command, operation string) bool {
	if command == "jobs" && operation == "summary-set" {
		return true
	}
	if command == "workers" && (operation == "pause" || operation == "resume") {
		return true
	}
	if command == "jobs" && (operation == "primary-set" || operation == "primary-clear") {
		return true
	}
	if command == "jobs" && (operation == "update" || operation == "checklist-set" || operation == "checklist-item") {
		return true
	}
	switch command + " " + operation {
	case "reviews publish", "reviews publication-recover", "jobs create", "inbox send", "inbox seen", "inbox ack", "reviews decide", "reviews fix", "reviews decide-batch", "reviews fix-batch", "reviews finalize", "reviews request", "reviews source-refresh", "reviews route-create", "reviews route-decide", "reviews route-fix":
		return true
	}
	return false
}

// Parse the complete request before loading a profile or credential. Explicit
// keys make retries recoverable across separate CLI processes without a global
// retry identity or silently regenerated key.
func parseMutation(command, operation string, args []string) (*mutationInvocation, bool) {
	if command == "jobs" && operation == "checklist-item" {
		return parseChecklistItem(args)
	}
	if command == "jobs" && operation == "summary-set" {
		return parseJobSummary(args)
	}
	if command == "workers" && (operation == "pause" || operation == "resume") {
		return parseWorkerPause(operation, args)
	}
	if command == "jobs" && (operation == "primary-set" || operation == "primary-clear") {
		return parsePrimaryJob(operation, args)
	}
	if command == "jobs" && (operation == "update" || operation == "checklist-set") {
		return parseJobProgress(operation, args)
	}
	if command == "reviews" {
		return parseReviewControl(operation, args)
	}
	call := &mutationInvocation{operation: command + " " + operation}
	allowed := map[string]bool{"--key": true}
	switch call.operation {
	case "jobs create":
		for _, flag := range []string{"--title", "--brief", "--brief-file", "--worker", "--status"} {
			allowed[flag] = true
		}
	case "inbox send":
		for _, flag := range []string{"--to", "--body", "--body-file", "--reply-to"} {
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
			if i >= len(args) {
				return nil, false
			}
			value = args[i]
		}
		if !utf8.ValidString(value) || value == "" && flag != "--brief" {
			return nil, false
		}
		values[flag] = value
	}
	call.key = values["--key"]
	if !validMutationKey(call.key) {
		return nil, false
	}
	wantsID := call.operation == "inbox seen" || call.operation == "inbox ack"
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
	files := map[string]int{}
	if call.operation == "jobs create" {
		files["--brief"] = 32 << 10
	} else if call.operation == "inbox send" {
		files["--body"] = 16 << 10
	}
	if !prepareTextFiles(values, files) {
		return nil, false
	}
	if call.operation == "jobs create" {
		if _, present := values["--title"]; !present {
			return nil, false
		}
	} else if call.operation == "inbox send" {
		if !validResourceID(values["--to"]) || strings.TrimSpace(values["--body"]) == "" || len(values["--body"]) > 16<<10 {
			return nil, false
		}
		if reply, present := values["--reply-to"]; present && !validResourceID(reply) {
			return nil, false
		}
	}
	if ok, failure := loadTextFiles(values, files); !ok {
		call.argumentDiagnostic = failure
		return call, false
	}
	switch call.operation {
	case "jobs create":
		if _, present := values["--title"]; !present {
			return nil, false
		}
		call.create = client.CreateJobRequest{Title: values["--title"], Brief: values["--brief"], WorkerID: values["--worker"], Status: values["--status"]}
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
	return client.ValidMutationKey(key)
}

func executeMutation(ctx context.Context, service *client.Client, call *mutationInvocation) (any, error) {
	switch call.operation {
	case "jobs summary-set":
		return service.SetJobSummary(ctx, call.key, call.id, call.summary)
	case "workers pause", "workers resume":
		return service.ReplaceWorkerPause(ctx, call.key, call.pause)
	case "jobs primary-set", "jobs primary-clear":
		return service.ReplacePrimaryJob(ctx, call.key, call.primary)
	case "jobs create":
		return service.CreateJob(ctx, call.key, call.create)
	case "jobs update":
		return service.UpdateJob(ctx, call.key, call.id, call.patch)
	case "jobs checklist-item":
		return service.UpdateJobChecklistItem(ctx, call.key, call.id, call.itemID, call.checklistItem)
	case "jobs checklist-set":
		return service.ReplaceJobChecklist(ctx, call.key, call.id, call.checklist)
	case "inbox send":
		return service.SendMessage(ctx, call.key, call.send)
	case "inbox seen":
		return service.MarkSeen(ctx, call.key, call.id)
	case "reviews request":
		return service.RequestReview(ctx, call.key, call.reviewRequest, call.reviewSourceWait)
	case "reviews publish":
		return service.RequestReviewCheckpoint(ctx, call.key, call.reviewCheckpoint, call.reviewSourceWait)
	case "reviews publication-recover":
		return service.RecoverReviewCheckpoint(ctx, call.key, call.id, call.reviewCheckpointRecovery, call.reviewSourceWait)
	case "reviews source-refresh":
		value, err := service.RefreshReviewSource(ctx, call.key, call.reviewSource, call.reviewSourceWait)
		return client.AgentReviewSourceResult(value), err
	case "reviews route-create":
		return service.CreateReviewFindingRoute(ctx, call.key, call.id, call.reviewRoute)
	case "reviews route-decide":
		return service.DecideReviewFindingRoute(ctx, call.key, call.id, call.reviewRouteDecision)
	case "reviews route-fix":
		return service.FixReviewFindingRoute(ctx, call.key, call.id, call.reviewRouteFix)
	case "reviews decide-batch":
		return service.DecideReviewFindings(ctx, call.key, call.id, call.reviewDecisionBatch)
	case "reviews fix-batch":
		return service.RecordReviewFixes(ctx, call.key, call.id, call.reviewFixBatch)
	case "reviews decide":
		return service.DecideReviewFinding(ctx, call.key, call.id, call.reviewDecision)
	case "reviews fix":
		return service.RecordReviewFix(ctx, call.key, call.id, call.reviewFix)
	case "reviews finalize":
		return service.FinalizeReviewSource(ctx, call.key, call.id, call.reviewFinalize)
	default:
		return service.Acknowledge(ctx, call.key, call.id)
	}
}
