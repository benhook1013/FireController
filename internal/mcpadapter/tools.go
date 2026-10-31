package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/buildinfo"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Quiet waits are finite and share the CLI result-wait ceiling.
const maxToolWaitSeconds = 900

type arguments struct {
	ownerManagement            *ownerManagementInvocation
	existingManagement         *existingManagementArgs
	subagentRecord             client.SubagentRecordRequest
	subagentUpdate             client.SubagentUpdateRequest
	queueChange                client.ReviewQueueChange
	reviewActiveQueue          bool
	summary                    client.SetJobSummaryRequest
	reviewRun                  client.ReviewRunRequest
	roundChannel               string
	roundControl               client.ConfigureReviewRoundControlRequest
	notes                      *noteToolCall
	projectMap                 *projectMapToolCall
	reviewCheckpoint           client.ReviewCheckpointRequest
	reviewCheckpointRecovery   client.ReviewCheckpointRecovery
	limit                      int
	waitMS                     int
	messageWaitSeconds         int
	cursor, worker, status, id string
	jobRevision                int64
	unread, unack, newest      bool
	snapshot                   string
	pullNumber                 int64
	key                        string
	create                     client.CreateJobRequest
	send                       client.SendMessageRequest
	patch                      client.UpdateJobRequest
	checklist                  client.ReplaceChecklistRequest
	checklistItem              client.UpdateChecklistItemRequest
	itemID                     string
	primary                    client.ReplacePrimaryJobRequest
	pause                      client.ReplaceWorkerPauseRequest
	reviewRoute                client.ReviewRouteRequest
	reviewRouteDecision        client.ReviewRouteDecisionRequest
	reviewRouteFix             client.ReviewRouteFixRequest
	reviewDecision             client.ReviewDecisionRequest
	reviewFix                  client.ReviewFixRequest
	reviewDecisionBatch        client.ReviewDecisionBatchRequest
	reviewFixBatch             client.ReviewFixBatchRequest
	operationStatus            *operationStatusInput
	reviewFinalize             client.ReviewFinalizeRequest
	reviewRequest              client.ReviewRequest
	reviewSource               client.ReviewSourceRequest
	reviewSourceWait           int
	reviewCIDiagnostics        bool
	reviewOperations           client.ReviewOperationsOptions
}
type definition struct {
	name, description string
	schema            map[string]any
}

func boundServer(binding profile.Binding) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "firecontroller", Version: Version}, &mcp.ServerOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}, PageSize: 128})
	definitions := append(toolDefinitions(), checkpointDefinitions()...)
	definitions = append(definitions, jobRevisionDefinition())
	definitions = append(definitions, roundControlDefinitions()...)
	definitions = append(definitions, reviewRunDefinitions()...)
	definitions = append(definitions, jobProgressDefinitions()...)
	definitions = append(definitions, jobSummaryDefinitions()...)
	definitions = append(definitions, primaryJobDefinitions()...)
	definitions = append(definitions, workerPauseDefinitions()...)
	definitions = append(definitions, noteDefinitions()...)
	definitions = append(definitions, projectMapDefinitions()...)
	definitions = append(definitions, messageWaitDefinition())
	definitions = append(definitions, operationListDefinition())
	definitions = append(definitions, jobStatusDefinitions()...)
	definitions = append(definitions, reviewBatchDefinitions()...)
	definitions = append(definitions, subagentDefinitions()...)
	definitions = append(definitions, queueChangeDefinitions()...)
	definitions = append(definitions, ownerManagementDefinitions()...)
	definitions = append(definitions, existingManagementDefinitions()...)
	known := make(map[string]bool)
	transports := sessionTransports{pools: make(map[*mcp.ServerSession]*client.TransportPool)}
	for _, tool := range definitions {
		known[tool.name] = true
		write := projectMapWrite(tool.name) || noteWrite(tool.name) || tool.name == "worker_pause_replace" || checkpointWrite(tool.name) || mutationTool(tool.name) || tool.name == "review_request" || tool.name == "review_source_refresh"
		openWorld := existingManagementExternal(tool.name) || checkpointWrite(tool.name) || tool.name == "review_start" || tool.name == "review_request" || tool.name == "review_source_refresh"
		destructive := existingManagementDestructive(tool.name) || ownerManagementDestructive(tool.name) || tool.name == "review_queue_move" || tool.name == "review_queue_remove" || tool.name == "review_subagent_update" || tool.name == "review_rounds_configure" || tool.name == "worker_pause_replace" || tool.name == "job_primary_replace" || tool.name == "map_entry_replace" || tool.name == "map_entry_update" || tool.name == "job_workstream_replace" || tool.name == "note_update" || tool.name == "job_update" || tool.name == "job_summary_set" || tool.name == "job_checklist_replace" || tool.name == "job_checklist_item_update" || reviewBatchTool(tool.name) || tool.name == "review_finding_decide" || tool.name == "review_finding_fix" || tool.name == "review_finding_route" || tool.name == "review_route_decide" || tool.name == "review_route_fix"
		s.AddTool(&mcp.Tool{Name: tool.name, Description: tool.description, InputSchema: tool.schema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: !write, IdempotentHint: true, DestructiveHint: &destructive, OpenWorldHint: &openWorld}}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args arguments
			var err error
			if projectMapTool(tool.name) {
				args, err = projectMapArguments(tool.name, request.Params.Arguments)
			} else if tool.name == "wait_for_messages" {
				args, err = messageWaitArguments(request.Params.Arguments)
			} else if noteTool(tool.name) {
				args, err = noteArguments(tool.name, request.Params.Arguments)
			} else if workerPauseTool(tool.name) {
				args, err = workerPauseArguments(tool.name, request.Params.Arguments)
			} else {
				args, err = parseArguments(tool.name, request.Params.Arguments)
			}
			if err != nil {
				return resultFailure(err), nil
			}
			if tool.name == "context" {
				return result(struct {
					profile.Context
					buildinfo.Connection
				}{binding.Context, buildinfo.ConnectionInfo(Version)}, false), nil
			}
			timeout := toolCallBudget(tool.name, args)
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			service, err := transports.newClient(ctx, binding, request.Session)
			if err != nil {
				return resultFailure(err), nil
			}
			defer service.Close()
			args = fitReviewWait(ctx, tool.name, args)
			options := client.PageOptions{Limit: args.limit, Cursor: args.cursor}
			var value any
			if existingManagementTool(tool.name) {
				value, err = executeExistingManagement(ctx, service, args.existingManagement)
			} else {
				switch tool.name {
				case "review_subagent_record":
					value, err = service.RecordSubagentReview(ctx, args.subagentRecord, args.key)
				case "review_subagent_read":
					value, err = service.ReadSubagentReview(ctx, args.id)
				case "review_subagent_update":
					value, err = service.UpdateSubagentReview(ctx, args.id, args.subagentUpdate, args.key)
				case "review_start":
					var result client.ReviewRunResult
					result, err = service.RunReview(ctx, args.key, args.reviewRun, args.reviewSourceWait)
					value = client.AgentReviewRunResult(result)
				case "review_selection_read":
					var selection client.ReviewRunState
					selection, err = service.WaitReviewSelection(ctx, args.id, args.reviewSourceWait)
					value = client.AgentReviewSelection(selection)
				case "review_rounds_read":
					value, err = service.ReviewRoundControl(ctx, args.pullNumber, args.roundChannel)
				case "review_rounds_configure":
					value, err = service.ConfigureReviewRoundControl(ctx, args.pullNumber, args.roundChannel, args.key, args.roundControl)
				case "map_entry_create", "map_entry_replace", "map_entry_update", "map_entry_list", "map_entry_read", "map_entry_history", "map_entry_revision_read", "job_workstream_read", "job_workstream_replace", "job_workstream_history":
					value, err = executeProjectMapTool(ctx, service, args.projectMap)
				case "wait_for_messages":
					value, err = service.WaitForMessages(ctx, args.cursor, args.limit, args.messageWaitSeconds)
				case "note_create", "note_update", "note_read", "note_list", "note_history", "note_revision_read":
					value, err = executeNoteTool(ctx, service, args.notes)
				case "presentation_read":
					value, err = service.Presentation(ctx)
				case "doctor":
					var pause client.WorkerPause
					if pause, err = service.WorkerPause(ctx); err == nil {
						value = struct {
							profile.Context
							Verified bool `json:"verified"`
							buildinfo.Connection
							WorkerPause client.WorkerPause `json:"worker_pause"`
						}{service.Identity(), true, buildinfo.ConnectionInfo(Version), pause}
					}
				case "review_queue_add", "review_queue_move", "review_queue_remove":
					value, err = service.ChangeReviewQueue(ctx, strings.TrimPrefix(tool.name, "review_queue_"), args.key, args.queueChange)
				case "review_operation_control", "review_policy_read", "review_policy_configure", "review_execution_configure", "review_attempt_settle", "worker_create", "project_credential_grant", "project_credential_revoke", "project_credential_update":
					value, err = executeOwnerManagement(ctx, service, args.ownerManagement)
				case "review_execution_read":
					value, err = service.ReviewExecution(ctx)
				case "review_operation_read":
					value, err = executeOperationStatus(ctx, service, args.operationStatus)
				case "review_operations_list":
					value, err = service.ReviewOperations(ctx, args.reviewOperations)
				case "review_request":
					var result client.ReviewRequestResult
					result, err = service.RequestReview(ctx, args.key, args.reviewRequest, args.reviewSourceWait)
					value = client.AgentReviewRequestResult(result)
				case "review_checkpoint_publish":
					value, err = service.RequestReviewCheckpoint(ctx, args.key, args.reviewCheckpoint, args.reviewSourceWait)
				case "review_checkpoint_read":
					value, err = service.ReviewCheckpointPublication(ctx, args.id)
				case "review_checkpoint_recover":
					value, err = service.RecoverReviewCheckpoint(ctx, args.key, args.id, args.reviewCheckpointRecovery, args.reviewSourceWait)
				case "review_source_refresh":
					var result client.ReviewSourceResult
					result, err = service.RefreshReviewSource(ctx, args.key, args.reviewSource, args.reviewSourceWait)
					value = client.AgentReviewSourceResult(result)
				case "review_source_refresh_read":
					var operation client.ReviewSourceOperation
					operation, err = service.ReviewSourceOperation(ctx, args.id)
					value = client.AgentReviewSourceOperation(operation)
				case "review_source_policy_assess":
					var assessment client.ReviewSourcePolicyAssessment
					assessment, err = service.ReviewSourcePolicyAssessment(ctx, args.pullNumber)
					value = client.AgentReviewSourcePolicy(assessment)
				case "review_source_readiness":
					var assessment client.ReviewSourceReadinessAssessment
					assessment, err = service.ReviewSourceReadiness(ctx, args.pullNumber)
					value = client.AgentReviewReadiness(assessment)
				case "review_source_ci_assess":
					var assessment client.ReviewSourceCIAssessment
					if args.reviewCIDiagnostics {
						assessment, err = service.ReviewSourceCIDiagnostics(ctx, args.pullNumber)
					} else {
						assessment, err = service.ReviewSourceCIAssessment(ctx, args.pullNumber)
					}
					value = client.AgentReviewCI(assessment)
				case "review_queue":
					if args.reviewActiveQueue {
						value, err = runReviewActiveQueue(ctx, service, options)
					} else {
						value, err = service.ReviewQueue(ctx, options)
					}
				case "review_pull":
					value, err = service.ReviewPull(ctx, args.pullNumber, args.snapshot)
				case "review_attempts":
					value, err = service.ReviewAttempts(ctx, args.pullNumber, client.ReviewAttemptOptions{PageOptions: options, Snapshot: args.snapshot})
				case "review_runs":
					var runs client.ReviewRuns
					runs, err = service.ReviewRuns(ctx, args.pullNumber, client.ReviewRunOptions{PageOptions: options, Snapshot: args.snapshot})
					value = client.AgentReviewRuns(runs)
				case "review_run":
					value, err = service.ReviewRun(ctx, args.id, args.snapshot)
				case "review_policy_assess":
					var assessment client.ReviewPolicyAssessment
					assessment, err = service.ReviewPolicyAssessment(ctx, args.id)
					value = client.AgentReviewPolicy(assessment)
				case "review_evidence_read":
					value, err = service.ReviewEvidence(ctx, args.id)
				case "review_adjudication_read":
					value, err = service.ReviewAdjudication(ctx, args.id)
				case "review_finding_route":
					value, err = service.CreateReviewFindingRoute(ctx, args.key, args.id, args.reviewRoute)
				case "review_route_read":
					value, err = service.ReviewFindingRoute(ctx, args.id)
				case "review_routes_list":
					value, err = service.ReviewFindingRoutes(ctx, args.pullNumber, options)
				case "review_route_decide":
					value, err = service.DecideReviewFindingRoute(ctx, args.key, args.id, args.reviewRouteDecision)
				case "review_route_fix":
					value, err = service.FixReviewFindingRoute(ctx, args.key, args.id, args.reviewRouteFix)
				case "review_findings_decide":
					value, err = service.DecideReviewFindings(ctx, args.key, args.id, args.reviewDecisionBatch)
				case "review_findings_fix":
					value, err = service.RecordReviewFixes(ctx, args.key, args.id, args.reviewFixBatch)
				case "review_finding_decide":
					value, err = service.DecideReviewFinding(ctx, args.key, args.id, args.reviewDecision)
				case "review_finding_fix":
					value, err = service.RecordReviewFix(ctx, args.key, args.id, args.reviewFix)
				case "review_source_finalize":
					value, err = service.FinalizeReviewSource(ctx, args.key, args.id, args.reviewFinalize)
				case "worker_pause_read":
					value, err = service.WorkerPause(ctx, args.worker)
				case "worker_pause_replace":
					value, err = service.ReplaceWorkerPause(ctx, args.key, args.pause, args.worker)
				case "worker_pause_history":
					value, err = service.WorkerPauseHistory(ctx, options, args.worker)
				case "workers_list":
					value, err = service.Workers(ctx, options)
				case "jobs_list":
					value, err = service.Jobs(ctx, options, client.JobFilter{WorkerID: args.worker, Status: args.status})
				case "jobs_assigned":
					value, err = service.Jobs(ctx, options, client.JobFilter{WorkerID: binding.Context.WorkerID, Status: args.status})
				case "job_primary_read":
					value, err = service.PrimaryJob(ctx, args.worker)
				case "job_primary_history":
					value, err = service.PrimaryJobHistory(ctx, options, args.worker)
				case "job_primary_replace":
					value, err = service.ReplacePrimaryJob(ctx, args.key, args.primary, args.worker)
				case "job_summary_read":
					value, err = service.JobSummary(ctx, args.id)
				case "job_summary_set":
					value, err = service.SetJobSummary(ctx, args.key, args.id, args.summary)
				case "job_status_read":
					value, err = runJobStatus(ctx, service, args)
				case "job_read":
					value, err = service.Job(ctx, args.id)
				case "job_revision_read":
					value, err = service.JobRevision(ctx, args.id, args.jobRevision)
				case "job_history":
					value, err = service.JobHistory(ctx, args.id, options)
				case "job_update":
					value, err = service.UpdateJob(ctx, args.key, args.id, args.patch)
				case "job_progress_history":
					value, err = service.JobProgressHistory(ctx, args.id, options)
				case "job_checklist_read":
					value, err = service.JobChecklist(ctx, args.id)
				case "job_checklist_replace":
					value, err = service.ReplaceJobChecklist(ctx, args.key, args.id, args.checklist)
				case "job_checklist_item_update":
					value, err = service.UpdateJobChecklistItem(ctx, args.key, args.id, args.itemID, args.checklistItem)
				case "job_checklist_history":
					value, err = service.JobChecklistHistory(ctx, args.id, options)
				case "outbox_list":
					value, err = service.Outbox(ctx, options, client.InboxFilter{Unread: args.unread, Unacknowledged: args.unack, Newest: args.newest}, args.worker)
				case "inbox_delivery_read":
					value, err = service.MessageDelivery(ctx, args.id)
				case "inbox_list":
					value, err = service.Inbox(ctx, options, client.InboxFilter{Unread: args.unread, Unacknowledged: args.unack, Newest: args.newest}, args.worker)
				case "inbox_snapshot":
					value, err = service.InboxSnapshot(ctx, options, client.InboxFilter{Unread: args.unread, Unacknowledged: args.unack, Newest: args.newest})
				case "inbox_events":
					value, err = service.MessageEvents(ctx, args.cursor, args.limit, args.waitMS)
				case "inbox_thread":
					value, err = service.Conversation(ctx, args.id, options)
				case "inbox_message_read":
					value, err = service.Message(ctx, args.id)
				case "jobs_create":
					value, err = service.CreateJob(ctx, args.key, args.create)
				case "inbox_send":
					value, err = service.SendMessage(ctx, args.key, args.send)
				case "inbox_seen":
					value, err = service.MarkSeen(ctx, args.key, args.id)
				case "inbox_ack":
					value, err = service.Acknowledge(ctx, args.key, args.id)
				}
			}
			if err != nil {
				return resultFailure(err), nil
			}
			return result(value, false), nil
		})
	}
	// Keep unknown tool errors concise rather than reflecting untrusted names.
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				if call, ok := request.(*mcp.CallToolRequest); ok && !known[call.Params.Name] {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "The requested tool is unavailable."}
				}
			}
			return next(ctx, method, request)
		}
	})
	return s
}

func result(value any, isError bool) *mcp.CallToolResult {
	data, err := json.Marshal(value)
	if err != nil || len(data) > 4<<20 {
		return resultFailure(diagnostic("output_invalid", "The tool result could not be encoded within its bound.", "Use a smaller explicit page or check service compatibility."))
	}
	return &mcp.CallToolResult{StructuredContent: value, Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}, IsError: isError}
}
func resultFailure(err error) *mcp.CallToolResult {
	d := diagnostic("service_unavailable", "The scoped operation could not be completed.", "Check the configured service and current access.")
	var failure *client.Error
	var local *Diagnostic
	if errors.As(err, &failure) {
		d = &Diagnostic{Code: failure.Code, Message: failure.Message, Action: failure.Action, Details: failure.Details, RequestID: failure.RequestID, OutcomeUnknown: failure.OutcomeUnknown}
	} else if errors.As(err, &local) {
		d = local
	}
	return result(struct {
		Error *Diagnostic `json:"error"`
	}{d}, true)
}

// Identity bootstrap may use ten seconds before the selected quiet wait; the
// client wait budget supplies five further seconds for response transport.
func toolCallBudget(name string, args arguments) time.Duration {
	timeout := 5 * time.Second
	switch name {
	case "wait_for_messages":
		timeout = client.MessageWaitBudget(args.messageWaitSeconds)
	case "review_source_refresh", "review_checkpoint_publish", "review_checkpoint_recover":
		timeout = client.SourceRefreshBudget(args.reviewSourceWait)
	case "review_start", "review_selection_read", "review_request", "review_operation_read":
		timeout = client.ReviewOperationBudget(args.reviewSourceWait)
	}
	return max(timeout+10*time.Second, 15*time.Second, existingManagementBudget(args.existingManagement))
}
