// Package cli provides local identity commands and bounded service
// commands for the explicitly selected profile.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/buildinfo"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

const (
	ExitSuccess    = 0
	ExitUsage      = 2
	ExitConfig     = 3
	ExitConflict   = 4
	ExitService    = 5
	ExitCancelled  = 6
	Version        = "0.1.0-dev"
	serviceTimeout = 5 * time.Second
)

type diagnostic struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	Action         string `json:"action"`
	Details        any    `json:"details,omitempty"`
	RequestID      string `json:"request_id,omitempty"`
	OutcomeUnknown bool   `json:"outcome_unknown,omitempty"`
}

type options struct {
	profile         string
	profilesFile    string
	hasProfile      bool
	hasProfilesFile bool
}

type invocation struct {
	options
	command               string
	subcommand            string
	position              []string
	limit                 int
	waitMS                int
	cursor                string
	worker                string
	status                string
	unread                bool
	unack                 bool
	snapshot              string
	pullNumber            int64
	mutation              *mutationInvocation
	setup                 *profileSetupInvocation
	notes                 *noteInvocation
	projectMap            *projectMapInvocation
	presentation          *presentationInvocation
	publication           *publicationInvocation
	helpOperation         string
	reviewOperations      client.ReviewOperationsOptions
	reviewOperationStatus *reviewOperationStatusInvocation
	reviewActiveQueue     bool

	reviewQueueEdit     *reviewQueueEditInvocation
	reviewImport        *reviewImportInvocation
	subagentReview      *subagentReviewInvocation
	reviewTransfer      *reviewTransferInvocation
	coordinationImport  *coordinationImportInvocation
	mapImport           *mapImportInvocation
	history             *historyInvocation
	reviewRun           *reviewRunInvocation
	rounds              *reviewRoundsInvocation
	roundAdoption       *roundAdoptionInvocation
	adoptionObligations *adoptionObligationsInvocation

	waitSeconds int
}

// Run preserves the original embedding API while RunContext adds caller
// cancellation for the executable and new service commands.
func Run(args []string, env map[string]string, stdout, stderr io.Writer) int {
	return RunContext(context.Background(), args, env, stdout, stderr)
}

// RunContext executes local commands without service I/O and service commands
// under one five-second budget covering identity verification and the operation.
func RunContext(ctx context.Context, args []string, env map[string]string, stdout, stderr io.Writer) int {
	call, ok := parseInvocation(args)
	if !ok {
		if call.mutation != nil && call.mutation.argumentDiagnostic != nil {
			return reportDetails(stderr, ExitUsage, *call.mutation.argumentDiagnostic)
		}
		return report(stderr, ExitUsage, "invalid_arguments", "The command or its arguments are not supported.", argumentHelpAction(call))
	}
	if call.history != nil && call.history.operation == "export-page" {
		if err := prepareHistoryExport(call.history.output); err != nil {
			return reportClientError(stderr, err, ctx)
		}
	}
	if call.command == "version" {
		return emit(stdout, stderr, buildinfo.Current(Version))
	}
	if call.command == "help" {
		if help, exists := operationHelp(call.helpOperation); exists {
			return emit(stdout, stderr, help)
		}
		return emit(stdout, stderr, localHelp())
	}
	if call.mapImport != nil {
		if err := prepareMapImport(ctx, call.mapImport); err != nil {
			return reportClientError(stderr, err, ctx)
		}
	}
	if call.projectMap != nil {
		if err := prepareProjectMap(ctx, call.projectMap); err != nil {
			return reportClientError(stderr, err, ctx)
		}
	}
	if call.presentation != nil {
		if err := preparePresentation(ctx, call.presentation); err != nil {
			return reportClientError(stderr, err, ctx)
		}
	}
	if call.publication != nil {
		if err := preparePublication(ctx, call.publication); err != nil {
			return reportClientError(stderr, err, ctx)
		}
	}
	if call.subagentReview != nil {
		if err := prepareSubagentReview(ctx, call.subagentReview); err != nil {
			return reportClientError(stderr, err, ctx)
		}
	}
	if call.reviewQueueEdit != nil {
		if err := prepareReviewQueueEdit(ctx, call.reviewQueueEdit); err != nil {
			return reportClientError(stderr, err, ctx)
		}
	}
	if call.reviewImport != nil {
		if err := prepareReviewImport(ctx, call.reviewImport); err != nil {
			return reportClientError(stderr, err, ctx)
		}
	}
	if call.reviewTransfer != nil {
		if err := prepareReviewTransfer(ctx, call.reviewTransfer); err != nil {
			return reportClientError(stderr, err, ctx)
		}
	}
	if call.roundAdoption != nil {
		if err := prepareRoundAdoption(ctx, call.roundAdoption); err != nil {
			return reportClientError(stderr, err, ctx)
		}
	}
	if call.adoptionObligations != nil {
		if err := prepareAdoptionObligations(ctx, call.adoptionObligations); err != nil {
			return reportClientError(stderr, err, ctx)
		}
	}
	if call.coordinationImport != nil {
		if err := prepareCoordinationImport(ctx, call.coordinationImport); err != nil {
			return reportClientError(stderr, err, ctx)
		}
	}
	if call.command == "profile" && call.subcommand == "create" {
		return createProfile(ctx, call, stdout, stderr)
	}
	if !call.hasProfile {
		call.profile = env["FC_PROFILE"]
	}
	if call.profile == "" || call.profile != strings.TrimSpace(call.profile) {
		return report(stderr, ExitConfig, "profile_required", "A profile name is required without surrounding whitespace.", "Set --profile NAME or FC_PROFILE.")
	}
	if !call.hasProfilesFile {
		directory := profile.ConfigDirectory(env)
		if directory == "" || !filepath.IsAbs(directory) {
			return report(stderr, ExitConfig, "config_directory_unavailable", "The user configuration directory could not be determined.", "Provide --profiles-file PATH or configure the user environment.")
		}
		call.profilesFile = filepath.Join(directory, "firecontroller", "profiles.json")
	}
	if call.command == "profile" {
		return createProfileLauncher(ctx, call, stdout, stderr)
	}
	if call.command == "context" {
		identity, err := profile.Load(call.profilesFile, call.profile)
		if err != nil {
			return report(stderr, ExitConfig, err.Code, err.Message, err.Action)
		}
		return emit(stdout, stderr, struct {
			profile.Context
			buildinfo.Connection
		}{identity, buildinfo.ConnectionInfo(Version)})
	}

	binding, profileErr := profile.LoadBinding(call.profilesFile, call.profile)
	if profileErr != nil {
		return report(stderr, ExitConfig, profileErr.Code, profileErr.Message, profileErr.Action)
	}
	if err := ctx.Err(); err != nil {
		return report(stderr, ExitCancelled, "cancelled", "The command was cancelled before connecting.", "No service operation was sent; retry when cancellation is no longer requested.")
	}
	timeout := serviceTimeout
	if call.history != nil && (call.subcommand == "collect" || call.subcommand == "collection") {
		timeout = time.Duration(call.history.waitSeconds+5) * time.Second
	}
	if call.command == "inbox" && call.subcommand == "wait" {
		timeout = client.MessageWaitBudget(call.waitSeconds)
	}
	if call.mutation != nil && (call.mutation.operation == "reviews source-refresh" || call.mutation.operation == "reviews publish" || call.mutation.operation == "reviews publication-recover") {
		timeout = client.SourceRefreshBudget(call.mutation.reviewSourceWait)
	}
	if call.mutation != nil && call.mutation.operation == "reviews request" {
		timeout = client.ReviewOperationBudget(call.mutation.reviewSourceWait)
	}
	if call.command == "reviews" && call.subcommand == "operation" {
		timeout = client.ReviewOperationBudget(call.waitSeconds)
	}
	if call.reviewRun != nil {
		timeout = client.ReviewOperationBudget(call.reviewRun.waitSeconds)
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	service, err := client.New(requestCtx, binding)
	if err != nil {
		return reportClientError(stderr, err, ctx)
	}
	defer service.Close()
	if call.reviewRun != nil {
		return executeReviewRun(requestCtx, ctx, service, call.reviewRun, stdout, stderr)
	}
	if call.roundAdoption != nil {
		return executeRoundAdoption(requestCtx, ctx, service, call.roundAdoption, stdout, stderr)
	}
	if call.adoptionObligations != nil {
		return executeAdoptionObligations(requestCtx, ctx, service, call.adoptionObligations, stdout, stderr)
	}
	if call.rounds != nil {
		return executeReviewRounds(requestCtx, ctx, service, call.rounds, stdout, stderr)
	}
	if call.history != nil {
		return executeHistory(requestCtx, ctx, service, call.history, stdout, stderr)
	}
	if call.mapImport != nil {
		return executeMapImport(requestCtx, ctx, service, call.mapImport, stdout, stderr)
	}
	if call.publication != nil {
		return executePublication(requestCtx, ctx, service, call.publication, stdout, stderr)
	}
	if call.presentation != nil {
		return executePresentation(requestCtx, ctx, service, call.presentation, stdout, stderr)
	}
	if call.subagentReview != nil {
		return executeSubagentReview(requestCtx, ctx, service, call.subagentReview, stdout, stderr)
	}
	if call.reviewQueueEdit != nil {
		return executeReviewQueueEdit(requestCtx, ctx, service, call.reviewQueueEdit, stdout, stderr)
	}
	if call.reviewImport != nil {
		return executeReviewImport(requestCtx, ctx, service, call.reviewImport, stdout, stderr)
	}
	if call.reviewTransfer != nil {
		return executeReviewTransfer(requestCtx, ctx, service, call.reviewTransfer, stdout, stderr)
	}
	if call.coordinationImport != nil {
		return executeCoordinationImport(requestCtx, ctx, service, call.coordinationImport, stdout, stderr)
	}
	if call.projectMap != nil {
		value, err := executeProjectMap(requestCtx, service, call.projectMap)
		if err != nil {
			return reportClientError(stderr, err, ctx)
		}
		if call.projectMap.write() {
			var encoded []byte
			encoded, err = json.Marshal(value)
			if err == nil {
				encoded = append(encoded, '\n')
				var written int
				written, err = stdout.Write(encoded)
				if err == nil && written != len(encoded) {
					err = io.ErrShortWrite
				}
			}
			if err != nil {
				return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The map mutation receipt could not be written.", Action: "Retry the exact same key, request and bound instance; do not use a replacement key.", OutcomeUnknown: true})
			}
			return ExitSuccess
		}
		return emit(stdout, stderr, value)
	}
	if call.notes != nil {
		value, err := executeNotes(requestCtx, service, call.notes)
		if err != nil {
			return reportClientError(stderr, err, ctx)
		}
		if call.notes.write() {
			if err = json.NewEncoder(stdout).Encode(value); err != nil {
				return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The note receipt could not be written.", Action: "Retry the exact same key, request and bound instance; do not use a replacement key.", OutcomeUnknown: true})
			}
			return ExitSuccess
		}
		return emit(stdout, stderr, value)
	}
	if call.mutation != nil {
		value, err := executeMutation(requestCtx, service, call.mutation)
		if err != nil {
			return reportClientError(stderr, err, ctx)
		}
		if err := json.NewEncoder(stdout).Encode(value); err != nil {
			return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The mutation receipt could not be written.", Action: "Recover by retrying the exact same key, request and bound instance; do not create a replacement key.", OutcomeUnknown: true})
		}
		return ExitSuccess
	}
	var value any
	switch {
	case call.command == "doctor":
		identity := service.Identity()
		if _, err = service.Worker(requestCtx); err != nil {
			return reportClientError(stderr, err, ctx)
		}
		value = struct {
			profile.Context
			Verified bool `json:"verified"`
			buildinfo.Connection
		}{identity, true, buildinfo.ConnectionInfo(Version)}
	case call.command == "reviews":
		value, err = executeReview(requestCtx, service, call)
	case call.command == "workers" && call.subcommand == "pause-state":
		value, err = service.WorkerPause(requestCtx)
	case call.command == "workers" && call.subcommand == "pause-history":
		value, err = service.WorkerPauseHistory(requestCtx, client.PageOptions{Limit: call.limit, Cursor: call.cursor})
	case call.command == "workers":
		value, err = service.Workers(requestCtx, client.PageOptions{Limit: call.limit, Cursor: call.cursor})
	case call.command == "jobs" && call.subcommand == "primary":
		value, err = service.PrimaryJob(requestCtx)
	case call.command == "jobs" && call.subcommand == "primary-history":
		value, err = service.PrimaryJobHistory(requestCtx, client.PageOptions{Limit: call.limit, Cursor: call.cursor})
	case call.command == "jobs" && call.subcommand == "list":
		value, err = service.Jobs(requestCtx, client.PageOptions{Limit: call.limit, Cursor: call.cursor}, client.JobFilter{WorkerID: call.worker, Status: call.status})
	case call.command == "jobs" && call.subcommand == "assigned":
		value, err = service.Jobs(requestCtx, client.PageOptions{Limit: call.limit, Cursor: call.cursor}, client.JobFilter{WorkerID: binding.Context.WorkerID, Status: call.status})
	case call.command == "jobs" && call.subcommand == "status":
		value, err = runJobStatus(requestCtx, service, call.position[0])
	case call.command == "jobs" && call.subcommand == "read":
		value, err = service.Job(requestCtx, call.position[0])
	case call.command == "jobs" && call.subcommand == "history":
		value, err = service.JobHistory(requestCtx, call.position[0], client.PageOptions{Limit: call.limit, Cursor: call.cursor})
	case call.command == "jobs" && call.subcommand == "progress-history":
		value, err = service.JobProgressHistory(requestCtx, call.position[0], client.PageOptions{Limit: call.limit, Cursor: call.cursor})
	case call.command == "jobs" && call.subcommand == "summary":
		value, err = service.JobSummary(requestCtx, call.position[0])
	case call.command == "jobs" && call.subcommand == "checklist":
		value, err = service.JobChecklist(requestCtx, call.position[0])
	case call.command == "jobs" && call.subcommand == "checklist-history":
		value, err = service.JobChecklistHistory(requestCtx, call.position[0], client.PageOptions{Limit: call.limit, Cursor: call.cursor})
	case call.command == "inbox" && call.subcommand == "list":
		value, err = service.Inbox(requestCtx, client.PageOptions{Limit: call.limit, Cursor: call.cursor}, client.InboxFilter{Unread: call.unread, Unacknowledged: call.unack})
	case call.command == "inbox" && call.subcommand == "snapshot":
		value, err = service.InboxSnapshot(requestCtx, client.PageOptions{Limit: call.limit, Cursor: call.cursor}, client.InboxFilter{Unread: call.unread, Unacknowledged: call.unack})
	case call.command == "inbox" && call.subcommand == "wait":
		value, err = service.WaitForMessages(requestCtx, call.cursor, call.limit, call.waitSeconds)
	case call.command == "inbox" && call.subcommand == "events":
		value, err = service.MessageEvents(requestCtx, call.cursor, call.limit, call.waitMS)
	case call.command == "inbox" && call.subcommand == "thread":
		value, err = service.Conversation(requestCtx, call.position[0], client.PageOptions{Limit: call.limit, Cursor: call.cursor})
	case call.command == "inbox" && call.subcommand == "read":
		value, err = service.Message(requestCtx, call.position[0])
	default:
		return report(stderr, ExitUsage, "invalid_arguments", "The command is not supported.", "Use a documented read command.")
	}
	if err != nil {
		return reportClientError(stderr, err, ctx)
	}
	return emit(stdout, stderr, value)
}

func parseInvocation(args []string) (invocation, bool) {
	var call invocation
	remaining := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		argument := args[i]
		flag, value, assigned := strings.Cut(argument, "=")
		if flag != "--profile" && flag != "--profiles-file" {
			remaining = append(remaining, argument)
			continue
		}
		if !assigned {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "--") {
				return call, false
			}
			value = args[i]
		}
		if value == "" {
			return call, false
		}
		if flag == "--profile" {
			if call.hasProfile {
				return call, false
			}
			call.profile, call.hasProfile = value, true
		} else {
			if call.hasProfilesFile {
				return call, false
			}
			call.profilesFile, call.hasProfilesFile = value, true
		}
	}
	if len(remaining) == 0 {
		return call, false
	}
	call.command = remaining[0]
	if call.command == "--help" || call.command == "-h" {
		call.command = "help"
	}
	if len(remaining) == 2 && (call.command == "reviews" && (remaining[1] == "help" || remaining[1] == "--help") || remaining[0] == "help" && remaining[1] == "reviews") {
		call.command, call.helpOperation = "help", "reviews help"
		return call, validHelpOptions(call.options)
	}
	if call.command == "version" || call.command == "help" {
		if remaining[0] == "help" && len(remaining) == 4 && remaining[1] == "reviews" && remaining[2] == "rounds" && (remaining[3] == "read" || remaining[3] == "grant" || remaining[3] == "close") {
			call.helpOperation = "reviews rounds " + remaining[3]
			return call, validHelpOptions(call.options)
		}
		if remaining[0] == "help" && len(remaining) == 3 {
			call.helpOperation = remaining[1] + " " + remaining[2]
			_, exists := operationHelp(call.helpOperation)
			return call, exists && validHelpOptions(call.options)
		}
		return call, len(remaining) == 1 && (!call.hasProfile || validSetupText(call.profile)) && (!call.hasProfilesFile || validSetupText(call.profilesFile))
	}
	if call.command == "context" || call.command == "doctor" {
		return call, len(remaining) == 1
	}
	if call.command == "profile" {
		if len(remaining) < 2 {
			return call, false
		}
		call.subcommand = remaining[1]
		var ok bool
		call.setup, ok = parseProfileSetup(call.subcommand, remaining[2:])
		return call, ok && (!call.hasProfile || validSetupText(call.profile)) && (!call.hasProfilesFile || validSetupText(call.profilesFile))
	}
	if call.command != "workers" && call.command != "jobs" && call.command != "inbox" && call.command != "reviews" && call.command != "notes" && call.command != "map" && call.command != "presentation" && call.command != "publication" && call.command != "history" && call.command != "import" || len(remaining) < 2 {
		return call, false
	}
	call.subcommand = remaining[1]
	operationArgs := remaining[2:]
	// Help is an exact grammar, not a scan for help-looking values. In
	// particular, --body --help remains ordinary submitted text.
	if call.command == "reviews" && call.subcommand == "rounds" && len(operationArgs) == 2 && (operationArgs[0] == "read" || operationArgs[0] == "grant" || operationArgs[0] == "close") && operationArgs[1] == "--help" {
		call.command, call.helpOperation = "help", "reviews rounds "+operationArgs[0]
		return call, validHelpOptions(call.options)
	}
	if len(operationArgs) == 1 && operationArgs[0] == "--help" {
		if _, exists := operationHelp(call.command + " " + call.subcommand); exists {
			call.helpOperation = call.command + " " + call.subcommand
			call.command = "help"
			return call, validHelpOptions(call.options)
		}
	}
	if call.command == "import" {
		return parseCoordinationImport(call, operationArgs)
	}
	if call.command == "history" {
		var ok bool
		call.history, ok = parseHistory(call.subcommand, operationArgs)
		return call, ok
	}
	if call.command == "map" && strings.HasPrefix(call.subcommand, "import-") {
		var ok bool
		call.mapImport, ok = parseMapImport(call.subcommand, operationArgs)
		return call, ok
	}
	if call.command == "publication" {
		var ok bool
		call.publication, ok = parsePublication(call.subcommand, operationArgs)
		return call, ok
	}
	if call.command == "presentation" {
		var ok bool
		call.presentation, ok = parsePresentation(call.subcommand, operationArgs)
		return call, ok
	}
	if call.command == "reviews" && call.subcommand == "rounds" {
		if len(operationArgs) > 0 && operationArgs[0] == "adopt" {
			return parseRoundAdoption(call, operationArgs[1:])
		}
		var ok bool
		call.rounds, ok = parseReviewRounds(operationArgs)
		return call, ok
	}
	if call.command == "reviews" && call.subcommand == "obligations" {
		var ok bool
		call.adoptionObligations, ok = parseAdoptionObligations(operationArgs)
		return call, ok
	}
	if call.command == "reviews" && (call.subcommand == "subagent-record" || call.subcommand == "subagent-read" || call.subcommand == "subagent-update") {
		return parseSubagentReview(call, operationArgs)
	}
	if call.command == "reviews" && call.subcommand == "queue-edit" {
		return parseReviewQueueEdit(call, operationArgs)
	}
	if call.command == "reviews" && (call.subcommand == "import-preview" || call.subcommand == "import-apply") {
		return parseReviewImport(call, operationArgs)
	}
	if call.command == "reviews" && (call.subcommand == "transfer-preview" || call.subcommand == "transfer-apply") {
		return parseReviewTransfer(call, operationArgs)
	}
	if call.command == "inbox" && call.subcommand == "wait" {
		return parseMessageWait(call, operationArgs)
	}
	if call.command == "map" || call.command == "jobs" && (call.subcommand == "workstream" || call.subcommand == "workstream-set" || call.subcommand == "workstream-history") {
		var ok bool
		call.projectMap, ok = parseProjectMap(call.command, call.subcommand, operationArgs)
		return call, ok
	}
	if call.command == "notes" {
		var ok bool
		call.notes, ok = parseNotes(call.subcommand, operationArgs)
		return call, ok
	}
	if isMutation(call.command, call.subcommand) {
		mutation, valid := parseMutation(call.command, call.subcommand, operationArgs)
		call.mutation = mutation
		return call, valid
	}
	if call.command == "reviews" {
		return parseReview(call, operationArgs)
	}
	allowed := map[string]bool{}
	switch call.command + " " + call.subcommand {
	case "workers pause-history", "jobs primary-history", "workers list", "jobs history", "jobs progress-history", "jobs checklist-history", "inbox thread":
		allowed["--limit"], allowed["--cursor"] = true, true
	case "jobs list":
		allowed["--limit"], allowed["--cursor"], allowed["--worker"], allowed["--status"] = true, true, true, true
	case "jobs assigned":
		allowed["--limit"], allowed["--cursor"], allowed["--status"], allowed["--all"] = true, true, true, true
		call.status = "open"
	case "inbox list":
		allowed["--limit"], allowed["--cursor"], allowed["--unread"], allowed["--unacknowledged"], allowed["--all"] = true, true, true, true, true
		call.unack = true
	case "inbox snapshot":
		allowed["--limit"], allowed["--cursor"], allowed["--unread"], allowed["--unacknowledged"], allowed["--all"] = true, true, true, true, true
		call.unack = true
	case "inbox events":
		allowed["--cursor"], allowed["--limit"], allowed["--wait-ms"] = true, true, true
	case "workers pause-state", "jobs primary", "jobs status", "jobs read", "jobs checklist", "jobs summary", "inbox read":
	default:
		return call, false
	}
	seen := make(map[string]bool)
	for i := 0; i < len(operationArgs); i++ {
		argument := operationArgs[i]
		if !strings.HasPrefix(argument, "-") {
			call.position = append(call.position, argument)
			continue
		}
		flag, value, assigned := strings.Cut(argument, "=")
		if !allowed[flag] || seen[flag] {
			return call, false
		}
		seen[flag] = true
		switch flag {
		case "--all":
			if assigned {
				return call, false
			}
			if call.command == "jobs" {
				call.status = ""
			} else {
				call.unack = false
			}
		case "--unread":
			if assigned {
				return call, false
			}
			call.unread = true
		case "--unacknowledged":
			if assigned {
				return call, false
			}
			call.unack = true
		default:
			if !assigned {
				i++
				if i >= len(operationArgs) {
					return call, false
				}
				value = operationArgs[i]
			}
			if value == "" {
				return call, false
			}
			switch flag {
			case "--limit":
				limit, err := strconv.Atoi(value)
				if err != nil || limit < 1 || limit > 100 {
					return call, false
				}
				call.limit = limit
			case "--wait-ms":
				waitMS, err := strconv.Atoi(value)
				if err != nil || waitMS < 0 || waitMS > 4000 {
					return call, false
				}
				call.waitMS = waitMS
			case "--cursor":
				if (call.command == "jobs" && call.subcommand == "primary-history" || call.command == "workers" && call.subcommand == "pause-history") && (value != strings.TrimSpace(value) || strings.IndexFunc(value, unicode.IsControl) >= 0) {
					return call, false
				}
				if len(value) > 4096 || !utf8.ValidString(value) {
					return call, false
				}
				if call.command == "inbox" && call.subcommand == "events" && (value != strings.TrimSpace(value) || strings.IndexFunc(value, unicode.IsControl) >= 0) {
					return call, false
				}
				call.cursor = value
			case "--worker":
				call.worker = value
			case "--status":
				if !client.ValidJobStatusFilter(value) {
					return call, false
				}
				call.status = value
			}
		}
	}
	if seen["--all"] && (seen["--unread"] || seen["--unacknowledged"] || seen["--status"]) {
		return call, false
	}
	expected := 0
	if call.command == "jobs" && (call.subcommand == "status" || call.subcommand == "read" || call.subcommand == "history" || call.subcommand == "progress-history" || call.subcommand == "checklist" || call.subcommand == "checklist-history" || call.subcommand == "summary") || call.command == "inbox" && (call.subcommand == "thread" || call.subcommand == "read") {
		expected = 1
	}
	if len(call.position) != expected {
		return call, false
	}
	if expected == 1 && !validResourceID(call.position[0]) {
		return call, false
	}
	if call.worker != "" && !validResourceID(call.worker) {
		return call, false
	}
	if call.command == "jobs" && call.subcommand == "assigned" && call.worker != "" {
		return call, false
	}
	if call.command == "inbox" && call.subcommand == "events" && !seen["--cursor"] {
		return call, false
	}
	return call, true
}

func validStatus(status string) bool {
	return client.ValidStatus(status)
}

func validResourceID(value string) bool {
	return client.ValidID(value)
}

func reportClientError(stderr io.Writer, err error, parent context.Context) int {
	var failure *client.Error
	if errors.As(err, &failure) {
		code := ExitService
		if parent.Err() != nil {
			code = ExitCancelled
		} else if failure.Code == "binding_invalid" || failure.Code == "credential_unreadable" || failure.Code == "credential_invalid" {
			code = ExitConfig
		} else if failure.Code == "instance_mismatch" || failure.Status == 409 {
			code = ExitConflict
		} else if failure.Status == 400 {
			code = ExitUsage
		}
		return reportDetails(stderr, code, diagnostic{Code: failure.Code, Message: failure.Message, Action: failure.Action, Details: failure.Details, RequestID: failure.RequestID, OutcomeUnknown: failure.OutcomeUnknown})
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if parent.Err() == nil {
			return report(stderr, ExitService, "request_timeout", "The service did not complete the read before its deadline.", "Retry the read when the service is responsive.")
		}
		return report(stderr, ExitCancelled, "cancelled", "The command was cancelled.", "Retry the read when cancellation is no longer requested.")
	}
	return report(stderr, ExitService, "service_unavailable", "The service read could not be completed.", "Check the configured service and try again.")
}

func reportDetails(stderr io.Writer, exitCode int, value diagnostic) int {
	_ = json.NewEncoder(stderr).Encode(struct {
		Error diagnostic `json:"error"`
	}{value})
	return exitCode
}

func emit(stdout, stderr io.Writer, value any) int {
	if err := json.NewEncoder(stdout).Encode(value); err != nil {
		return report(stderr, ExitConfig, "output_failed", "The response could not be written.", "Check the output destination before retrying.")
	}
	return ExitSuccess
}

func report(stderr io.Writer, exitCode int, code, message, action string) int {
	return reportDetails(stderr, exitCode, diagnostic{Code: code, Message: message, Action: action})
}
