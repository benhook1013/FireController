package cli

import (
	"context"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
)

func parseReview(call invocation, args []string) (invocation, bool) {
	if call.subcommand == "operations" {
		return parseOperationList(call, args)
	}
	if call.subcommand == "start" || call.subcommand == "run-read" {
		return parseReviewRun(call, args)
	}
	if call.subcommand == "source-assess" || call.subcommand == "ci-assess" || call.subcommand == "readiness" {
		values, ok := reviewSourceFlags(args, "--pr")
		if !ok {
			return call, false
		}
		number, err := strconv.ParseInt(values["--pr"], 10, 64)
		call.pullNumber = number
		return call, err == nil && number >= 1 && number <= 2147483647 && strconv.FormatInt(number, 10) == values["--pr"]
	}
	list := call.subcommand == "queue" || call.subcommand == "runs" || call.subcommand == "attempts" || call.subcommand == "routes"
	switch call.subcommand {
	case "queue", "pull", "runs", "attempts", "run", "evidence", "adjudication", "operation", "assess", "source-operation", "route", "routes", "publication":
	case "execution":
		return call, len(args) == 0
	default:
		return call, false
	}
	if list {
		call.limit = 5
	}
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if !strings.HasPrefix(argument, "-") {
			call.position = append(call.position, argument)
			continue
		}
		flag, value, assigned := strings.Cut(argument, "=")
		if seen[flag] || !(list && (flag == "--limit" || flag == "--cursor") || call.subcommand == "routes" && flag == "--pr" || call.subcommand == "operation" && flag == "--wait-seconds" || (call.subcommand == "pull" || call.subcommand == "runs" || call.subcommand == "attempts" || call.subcommand == "run") && flag == "--snapshot") {
			return call, false
		}
		seen[flag] = true
		if !assigned {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "-") {
				return call, false
			}
			value = args[i]
		}
		switch flag {
		case "--wait-seconds":
			seconds, err := strconv.Atoi(value)
			if err != nil || strconv.Itoa(seconds) != value || client.ValidateReviewOperationWait(seconds) != nil {
				return call, false
			}
			call.waitSeconds = seconds
		case "--pr":
			number, err := strconv.ParseInt(value, 10, 64)
			if err != nil || number < 1 || number > 2147483647 || strconv.FormatInt(number, 10) != value {
				return call, false
			}
			call.pullNumber = number
		case "--limit":
			limit, err := strconv.Atoi(value)
			if err != nil || limit < 1 || limit > 100 {
				return call, false
			}
			call.limit = limit
		case "--cursor":
			if value == "" || len(value) > 4096 || !utf8.ValidString(value) || value != strings.TrimSpace(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
				return call, false
			}
			call.cursor = value
		case "--snapshot":
			if !reviewID(value, "review_snapshot_") {
				return call, false
			}
			call.snapshot = value
		}
	}
	if call.subcommand == "routes" {
		return call, len(call.position) == 0 && call.pullNumber > 0
	}
	if call.subcommand == "queue" {
		return call, len(call.position) == 0
	}
	if len(call.position) != 1 {
		return call, false
	}
	if call.subcommand == "publication" {
		return call, client.ValidateReviewCheckpointID(call.position[0]) == nil
	}
	if call.subcommand == "route" {
		return call, client.ValidateReviewRouteID(call.position[0]) == nil
	}
	if call.subcommand == "operation" {
		return call, client.ValidateReviewOperationID(call.position[0]) == nil
	}
	if call.subcommand == "source-operation" {
		return call, client.ValidateReviewSourceRefreshID(call.position[0]) == nil
	}
	if call.subcommand == "run" {
		return call, reviewID(call.position[0], "review_run_")
	}
	if call.subcommand == "evidence" || call.subcommand == "adjudication" || call.subcommand == "assess" {
		return call, client.ValidateReviewEvidenceID(call.position[0]) == nil
	}
	number, err := strconv.ParseInt(call.position[0], 10, 64)
	if err != nil || number < 1 || number > 2147483647 || strconv.FormatInt(number, 10) != call.position[0] {
		return call, false
	}
	call.pullNumber = number
	return call, true
}

func reviewID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+32 {
		return false
	}
	for _, c := range value[len(prefix):] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func executeReview(ctx context.Context, service *client.Client, call invocation) (any, error) {
	switch call.subcommand {
	case "operations":
		return service.ReviewOperations(ctx, call.reviewOperations)
	case "publication":
		return service.ReviewCheckpointPublication(ctx, call.position[0])
	case "route":
		return service.ReviewFindingRoute(ctx, call.position[0])
	case "routes":
		return service.ReviewFindingRoutes(ctx, call.pullNumber, client.PageOptions{Limit: call.limit, Cursor: call.cursor})
	case "execution":
		return service.ReviewExecution(ctx)
	case "operation":
		return service.WaitReviewOperation(ctx, call.position[0], call.waitSeconds)
	case "source-operation":
		return service.ReviewSourceOperation(ctx, call.position[0])
	case "source-assess":
		return service.ReviewSourcePolicyAssessment(ctx, call.pullNumber)
	case "readiness":
		return service.ReviewSourceReadiness(ctx, call.pullNumber)
	case "ci-assess":
		return service.ReviewSourceCIAssessment(ctx, call.pullNumber)
	case "queue":
		return service.ReviewQueue(ctx, client.PageOptions{Limit: call.limit, Cursor: call.cursor})
	case "pull":
		return service.ReviewPull(ctx, call.pullNumber, call.snapshot)
	case "attempts":
		return service.ReviewAttempts(ctx, call.pullNumber, client.ReviewAttemptOptions{PageOptions: client.PageOptions{Limit: call.limit, Cursor: call.cursor}, Snapshot: call.snapshot})
	case "runs":
		return service.ReviewRuns(ctx, call.pullNumber, client.ReviewRunOptions{PageOptions: client.PageOptions{Limit: call.limit, Cursor: call.cursor}, Snapshot: call.snapshot})
	case "evidence":
		return service.ReviewEvidence(ctx, call.position[0])
	case "adjudication":
		return service.ReviewAdjudication(ctx, call.position[0])
	case "assess":
		return service.ReviewPolicyAssessment(ctx, call.position[0])
	default: // parseReview admits only the explicit run-detail command here.
		return service.ReviewRun(ctx, call.position[0], call.snapshot)
	}
}

func parseReviewControl(operation string, args []string) (*mutationInvocation, bool) {
	if operation == "publish" || operation == "publication-recover" {
		return parseReviewPublication(operation, args)
	}
	if operation == "source-refresh" {
		waitSeconds := 2
		var filtered []string
		seenWait := false
		includeCI := false
		includeReadiness := false
		for i := 0; i < len(args); i++ {
			flag, value, assigned := strings.Cut(args[i], "=")
			if flag == "--readiness" {
				if assigned || includeReadiness {
					return nil, false
				}
				includeReadiness = true
				continue
			}
			if flag == "--ci" {
				if assigned || includeCI {
					return nil, false
				}
				includeCI = true
				continue
			}
			if flag != "--wait-seconds" {
				filtered = append(filtered, args[i])
				continue
			}
			if seenWait {
				return nil, false
			}
			seenWait = true
			if !assigned {
				i++
				if i >= len(args) {
					return nil, false
				}
				value = args[i]
			}
			n, err := strconv.Atoi(value)
			if err != nil || strconv.Itoa(n) != value || client.ValidateReviewSourceWait(n) != nil {
				return nil, false
			}
			waitSeconds = n
		}
		values, ok := reviewSourceFlags(filtered, "--pr", "--head", "--expect-config", "--key")
		if !ok || !validMutationKey(values["--key"]) {
			return nil, false
		}
		number, err := strconv.ParseInt(values["--pr"], 10, 64)
		if err != nil || strconv.FormatInt(number, 10) != values["--pr"] {
			return nil, false
		}
		revision, err := strconv.ParseInt(values["--expect-config"], 10, 64)
		request := client.ReviewSourceRequest{ExpectedConfigurationRevision: revision, PullNumber: number, HeadSHA: values["--head"], IncludeCI: includeCI, IncludeReadiness: includeReadiness}
		if err != nil || strconv.FormatInt(revision, 10) != values["--expect-config"] || client.ValidateReviewSourceRequest(request) != nil {
			return nil, false
		}
		return &mutationInvocation{operation: "reviews source-refresh", key: values["--key"], reviewSource: request, reviewSourceWait: waitSeconds}, true
	}
	if operation == "request" {
		return parseReviewRequest(args)
	}
	call := &mutationInvocation{operation: "reviews " + operation}
	allowed := map[string]bool{"--key": true, "--expect-revision": true}
	switch operation {
	case "decide":
		allowed["--expect-route-revision"] = true
		for _, flag := range []string{"--finding", "--disposition", "--reason"} {
			allowed[flag] = true
		}
	case "fix":
		for _, flag := range []string{"--finding", "--expect-sha", "--fix-sha", "--proof"} {
			allowed[flag] = true
		}
	case "route-create":
		for _, flag := range []string{"--finding", "--target-pr", "--reason", "--expect-route-revision"} {
			allowed[flag] = true
		}
	case "route-decide":
		allowed["--disposition"], allowed["--reason"] = true, true
	case "route-fix":
		allowed["--expect-sha"], allowed["--fix-sha"], allowed["--proof"] = true, true, true
	case "finalize":
	default:
		return nil, false
	}
	values := make(map[string]string)
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
		if _, exists := values[flag]; exists {
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
	if len(positions) != 1 || !validMutationKey(values["--key"]) {
		return nil, false
	}
	if operation == "route-decide" || operation == "route-fix" {
		if client.ValidateReviewRouteID(positions[0]) != nil {
			return nil, false
		}
	} else if client.ValidateReviewEvidenceID(positions[0]) != nil {
		return nil, false
	}
	// Presence is independent of value: index zero and an empty prior SHA are
	// legitimate explicit assertions, never implicit defaults.
	for flag := range allowed {
		if flag == "--expect-route-revision" {
			continue
		}
		if _, present := values[flag]; !present {
			return nil, false
		}
	}
	revision, err := strconv.ParseInt(values["--expect-revision"], 10, 64)
	if err != nil || revision < 0 || revision == (1<<63)-1 || strconv.FormatInt(revision, 10) != values["--expect-revision"] {
		return nil, false
	}
	var routeRevision int64
	if value, present := values["--expect-route-revision"]; present {
		routeRevision, err = strconv.ParseInt(value, 10, 64)
		if err != nil || routeRevision < 0 || routeRevision == (1<<63)-1 || strconv.FormatInt(routeRevision, 10) != value {
			return nil, false
		}
	}
	var finding int64
	if operation == "decide" || operation == "fix" || operation == "route-create" {
		finding, err = strconv.ParseInt(values["--finding"], 10, 64)
		if err != nil || finding < 0 || finding >= 100 || strconv.FormatInt(finding, 10) != values["--finding"] {
			return nil, false
		}
	}
	call.id, call.key = positions[0], values["--key"]
	switch operation {
	case "decide":
		call.reviewDecision = client.ReviewDecisionRequest{ExpectedRouteRevision: routeRevision, ExpectedRevision: revision, Finding: finding, Disposition: values["--disposition"], Reason: values["--reason"]}
		if client.ValidateReviewDecisionRequest(call.reviewDecision) != nil {
			return nil, false
		}
	case "fix":
		call.reviewFix = client.ReviewFixRequest{ExpectedRevision: revision, Finding: finding, ExpectedSHA: values["--expect-sha"], FixSHA: values["--fix-sha"], ProofNote: values["--proof"]}
		if client.ValidateReviewFixRequest(call.reviewFix) != nil {
			return nil, false
		}
	case "route-create":
		target, err := strconv.ParseInt(values["--target-pr"], 10, 64)
		if err != nil || strconv.FormatInt(target, 10) != values["--target-pr"] {
			return nil, false
		}
		call.reviewRoute = client.ReviewRouteRequest{ExpectedRevision: revision, ExpectedRouteRevision: routeRevision, Finding: finding, TargetPullNumber: target, Reason: values["--reason"]}
		if client.ValidateReviewRouteRequest(call.reviewRoute) != nil {
			return nil, false
		}
	case "route-decide":
		call.reviewRouteDecision = client.ReviewRouteDecisionRequest{ExpectedRevision: revision, Disposition: values["--disposition"], Reason: values["--reason"]}
		if client.ValidateReviewRouteDecisionRequest(call.reviewRouteDecision) != nil {
			return nil, false
		}
	case "route-fix":
		call.reviewRouteFix = client.ReviewRouteFixRequest{ExpectedRevision: revision, ExpectedSHA: values["--expect-sha"], FixSHA: values["--fix-sha"], ProofNote: values["--proof"]}
		if client.ValidateReviewRouteFixRequest(call.reviewRouteFix) != nil {
			return nil, false
		}
	case "finalize":
		call.reviewFinalize = client.ReviewFinalizeRequest{ExpectedRevision: revision}
		if client.ValidateReviewFinalizeRequest(call.reviewFinalize) != nil {
			return nil, false
		}
	}
	return call, true
}

func reviewSourceFlags(args []string, flags ...string) (map[string]string, bool) {
	allowed := make(map[string]bool, len(flags))
	for _, flag := range flags {
		allowed[flag] = true
	}
	values := make(map[string]string, len(flags))
	for i := 0; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
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
		values[flag] = value
	}
	return values, len(values) == len(flags)
}

func parseReviewRequest(args []string) (*mutationInvocation, bool) {
	allowed := map[string]bool{"--key": true, "--expect-config": true, "--channel": true, "--head": true, "--wait-seconds": true, "--out-of-order": true}
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
		if flag == "--out-of-order" {
			if !assigned {
				value = "true"
			}
			if value != "true" && value != "false" {
				return nil, false
			}
			values[flag] = value
			continue
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
	if len(positions) != 1 || len(values) < 4 || len(values) > 6 || !validMutationKey(values["--key"]) {
		return nil, false
	}
	number, err := strconv.ParseInt(positions[0], 10, 64)
	if err != nil || strconv.FormatInt(number, 10) != positions[0] {
		return nil, false
	}
	revision, err := strconv.ParseInt(values["--expect-config"], 10, 64)
	if err != nil || strconv.FormatInt(revision, 10) != values["--expect-config"] {
		return nil, false
	}
	seconds := 2
	if raw, present := values["--wait-seconds"]; present {
		parsed, err := strconv.Atoi(raw)
		if err != nil || strconv.Itoa(parsed) != raw || client.ValidateReviewOperationWait(parsed) != nil {
			return nil, false
		}
		seconds = parsed
	}
	request := client.ReviewRequest{ExpectedConfigurationRevision: revision, PullNumber: number, Channel: values["--channel"], HeadSHA: values["--head"], AllowOutOfOrder: values["--out-of-order"] == "true"}
	if client.ValidateReviewRequest(request) != nil {
		return nil, false
	}
	return &mutationInvocation{operation: "reviews request", key: values["--key"], reviewRequest: request, reviewSourceWait: seconds}, true
}

func parseReviewPublication(operation string, args []string) (*mutationInvocation, bool) {
	if len(args) < 1 {
		return nil, false
	}
	id := args[0]
	recovery := operation == "publication-recover"
	if recovery && client.ValidateReviewCheckpointID(id) != nil || !recovery && client.ValidateReviewEvidenceID(id) != nil {
		return nil, false
	}
	values := map[string]string{}
	retry := false
	for i := 1; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
		if flag == "--retry-unposted" {
			if !recovery || assigned || retry {
				return nil, false
			}
			retry = true
			continue
		}
		if flag != "--key" && flag != "--expect-revision" && flag != "--wait-seconds" {
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
	revision, err := strconv.ParseInt(values["--expect-revision"], 10, 64)
	if err != nil || strconv.FormatInt(revision, 10) != values["--expect-revision"] || !validMutationKey(values["--key"]) {
		return nil, false
	}
	wait := 2
	if value, ok := values["--wait-seconds"]; ok {
		n, e := strconv.Atoi(value)
		if e != nil || strconv.Itoa(n) != value || client.ValidateReviewSourceWait(n) != nil {
			return nil, false
		}
		wait = n
	}
	call := &mutationInvocation{operation: "reviews " + operation, id: id, key: values["--key"], reviewSourceWait: wait}
	if recovery {
		call.reviewCheckpointRecovery = client.ReviewCheckpointRecovery{ExpectedRevision: revision, RetryUnposted: retry}
		return call, client.ValidateReviewCheckpointRecovery(call.reviewCheckpointRecovery) == nil
	}
	call.reviewCheckpoint = client.ReviewCheckpointRequest{EvidenceID: id, ExpectedRevision: revision}
	return call, client.ValidateReviewCheckpointRequest(call.reviewCheckpoint) == nil
}
