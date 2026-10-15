package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type arguments struct {
	reviewCheckpoint           client.ReviewCheckpointRequest
	reviewCheckpointRecovery   client.ReviewCheckpointRecovery
	limit                      int
	waitMS                     int
	cursor, worker, status, id string
	unread, unack              bool
	snapshot                   string
	pullNumber                 int64
	key                        string
	create                     client.CreateJobRequest
	edit                       client.EditJobRequest
	send                       client.SendMessageRequest
	checkpoint                 client.AppendCheckpointRequest
	update                     client.AppendUpdateRequest
	checklist                  client.ReplaceChecklistRequest
	primary                    client.ReplacePrimaryJobRequest
	reviewRoute                client.ReviewRouteRequest
	reviewRouteDecision        client.ReviewRouteDecisionRequest
	reviewRouteFix             client.ReviewRouteFixRequest
	reviewDecision             client.ReviewDecisionRequest
	reviewFix                  client.ReviewFixRequest
	reviewFinalize             client.ReviewFinalizeRequest
	reviewRequest              client.ReviewRequest
	reviewSource               client.ReviewSourceRequest
	reviewSourceWait           int
}
type definition struct {
	name, description string
	schema            map[string]any
}

func boundServer(binding profile.Binding) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "firecontroller", Version: Version}, &mcp.ServerOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}, PageSize: 64})
	definitions := append(toolDefinitions(), checkpointDefinitions()...)
	definitions = append(definitions, jobProgressDefinitions()...)
	definitions = append(definitions, primaryJobDefinitions()...)
	known := make(map[string]bool)
	for _, tool := range definitions {
		known[tool.name] = true
		write := checkpointWrite(tool.name) || mutationTool(tool.name) || tool.name == "review_request" || tool.name == "review_source_refresh"
		openWorld := checkpointWrite(tool.name) || tool.name == "review_request" || tool.name == "review_source_refresh"
		destructive := tool.name == "job_edit" || tool.name == "job_checklist_replace" || tool.name == "review_finding_decide" || tool.name == "review_finding_fix" || tool.name == "review_finding_route" || tool.name == "review_route_decide" || tool.name == "review_route_fix"
		s.AddTool(&mcp.Tool{Name: tool.name, Description: tool.description, InputSchema: tool.schema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: !write, IdempotentHint: true, DestructiveHint: &destructive, OpenWorldHint: &openWorld}}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args, err := parseArguments(tool.name, request.Params.Arguments)
			if err != nil {
				return resultFailure(err), nil
			}
			if tool.name == "context" {
				return result(binding.Context, false), nil
			}
			timeout := 5 * time.Second
			if (tool.name == "review_source_refresh" || checkpointWrite(tool.name)) && args.reviewSourceWait > 2 {
				timeout = time.Duration(args.reviewSourceWait+5) * time.Second
			}
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			service, err := client.New(ctx, binding)
			if err != nil {
				return resultFailure(err), nil
			}
			defer service.Close()
			options := client.PageOptions{Limit: args.limit, Cursor: args.cursor}
			var value any
			switch tool.name {
			case "doctor":
				if _, err = service.Worker(ctx); err == nil {
					value = struct {
						profile.Context
						Verified bool `json:"verified"`
					}{service.Identity(), true}
				}
			case "review_execution_read":
				value, err = service.ReviewExecution(ctx)
			case "review_operation_read":
				value, err = service.ReviewOperation(ctx, args.id)
			case "review_request":
				value, err = service.RequestReview(ctx, args.key, args.reviewRequest)
			case "review_checkpoint_publish":
				value, err = service.RequestReviewCheckpoint(ctx, args.key, args.reviewCheckpoint, args.reviewSourceWait)
			case "review_checkpoint_read":
				value, err = service.ReviewCheckpointPublication(ctx, args.id)
			case "review_checkpoint_recover":
				value, err = service.RecoverReviewCheckpoint(ctx, args.key, args.id, args.reviewCheckpointRecovery, args.reviewSourceWait)
			case "review_source_refresh":
				value, err = service.RefreshReviewSource(ctx, args.key, args.reviewSource, args.reviewSourceWait)
			case "review_source_refresh_read":
				value, err = service.ReviewSourceOperation(ctx, args.id)
			case "review_source_policy_assess":
				value, err = service.ReviewSourcePolicyAssessment(ctx, args.pullNumber)
			case "review_source_readiness":
				value, err = service.ReviewSourceReadiness(ctx, args.pullNumber)
			case "review_source_ci_assess":
				value, err = service.ReviewSourceCIAssessment(ctx, args.pullNumber)
			case "review_queue":
				value, err = service.ReviewQueue(ctx, options)
			case "review_pull":
				value, err = service.ReviewPull(ctx, args.pullNumber, args.snapshot)
			case "review_runs":
				value, err = service.ReviewRuns(ctx, args.pullNumber, client.ReviewRunOptions{PageOptions: options, Snapshot: args.snapshot})
			case "review_run":
				value, err = service.ReviewRun(ctx, args.id, args.snapshot)
			case "review_policy_assess":
				value, err = service.ReviewPolicyAssessment(ctx, args.id)
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
			case "review_finding_decide":
				value, err = service.DecideReviewFinding(ctx, args.key, args.id, args.reviewDecision)
			case "review_finding_fix":
				value, err = service.RecordReviewFix(ctx, args.key, args.id, args.reviewFix)
			case "review_source_finalize":
				value, err = service.FinalizeReviewSource(ctx, args.key, args.id, args.reviewFinalize)
			case "workers_list":
				value, err = service.Workers(ctx, options)
			case "jobs_list":
				value, err = service.Jobs(ctx, options, client.JobFilter{WorkerID: args.worker, Status: args.status})
			case "jobs_assigned":
				value, err = service.Jobs(ctx, options, client.JobFilter{WorkerID: binding.Context.WorkerID, Status: args.status})
			case "job_primary_read":
				value, err = service.PrimaryJob(ctx)
			case "job_primary_history":
				value, err = service.PrimaryJobHistory(ctx, options)
			case "job_primary_replace":
				value, err = service.ReplacePrimaryJob(ctx, args.key, args.primary)
			case "job_read":
				value, err = service.Job(ctx, args.id)
			case "job_history":
				value, err = service.JobHistory(ctx, args.id, options)
			case "job_checkpoint_latest":
				value, err = service.LatestCheckpoint(ctx, args.id)
			case "job_checkpoint_history":
				value, err = service.CheckpointHistory(ctx, args.id, options)
			case "job_checkpoint_append":
				value, err = service.AppendCheckpoint(ctx, args.key, args.id, args.checkpoint)
			case "job_update_append":
				value, err = service.AppendJobUpdate(ctx, args.key, args.id, args.update)
			case "job_update_history":
				value, err = service.JobUpdates(ctx, args.id, options)
			case "job_checklist_read":
				value, err = service.JobChecklist(ctx, args.id)
			case "job_checklist_replace":
				value, err = service.ReplaceJobChecklist(ctx, args.key, args.id, args.checklist)
			case "job_checklist_history":
				value, err = service.JobChecklistHistory(ctx, args.id, options)
			case "inbox_list":
				value, err = service.Inbox(ctx, options, client.InboxFilter{Unread: args.unread, Unacknowledged: args.unack})
			case "inbox_snapshot":
				value, err = service.InboxSnapshot(ctx, options, client.InboxFilter{Unread: args.unread, Unacknowledged: args.unack})
			case "inbox_events":
				value, err = service.MessageEvents(ctx, args.cursor, args.limit, args.waitMS)
			case "inbox_thread":
				value, err = service.Conversation(ctx, args.id, options)
			case "jobs_create":
				value, err = service.CreateJob(ctx, args.key, args.create)
			case "job_edit":
				value, err = service.EditJob(ctx, args.key, args.id, args.edit)
			case "inbox_send":
				value, err = service.SendMessage(ctx, args.key, args.send)
			case "inbox_seen":
				value, err = service.MarkSeen(ctx, args.key, args.id)
			case "inbox_ack":
				value, err = service.Acknowledge(ctx, args.key, args.id)
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
