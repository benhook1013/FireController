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
	limit                      int
	cursor, worker, status, id string
	unread, unack              bool
}
type definition struct {
	name, description string
	schema            map[string]any
}

func boundServer(binding profile.Binding) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "firecontroller", Version: Version}, &mcp.ServerOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}, PageSize: 20})
	definitions := toolDefinitions()
	known := make(map[string]bool)
	for _, tool := range definitions {
		known[tool.name] = true
		no := false
		openWorld := tool.name != "context"
		s.AddTool(&mcp.Tool{Name: tool.name, Description: tool.description, InputSchema: tool.schema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: &no, OpenWorldHint: &openWorld}}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args, err := parseArguments(tool.name, request.Params.Arguments)
			if err != nil {
				return resultFailure(err), nil
			}
			if tool.name == "context" {
				return result(binding.Context, false), nil
			}
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
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
			case "workers_list":
				value, err = service.Workers(ctx, options)
			case "jobs_list":
				value, err = service.Jobs(ctx, options, client.JobFilter{WorkerID: args.worker, Status: args.status})
			case "jobs_assigned":
				value, err = service.Jobs(ctx, options, client.JobFilter{WorkerID: binding.Context.WorkerID, Status: args.status})
			case "job_read":
				value, err = service.Job(ctx, args.id)
			case "job_history":
				value, err = service.JobHistory(ctx, args.id, options)
			case "inbox_list":
				value, err = service.Inbox(ctx, options, client.InboxFilter{Unread: args.unread, Unacknowledged: args.unack})
			case "inbox_thread":
				value, err = service.Conversation(ctx, args.id, options)
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
	d := diagnostic("service_unavailable", "The scoped read could not be completed.", "Check the configured service and current access.")
	var failure *client.Error
	var local *Diagnostic
	if errors.As(err, &failure) {
		d = &Diagnostic{Code: failure.Code, Message: failure.Message, Action: failure.Action, Details: failure.Details, RequestID: failure.RequestID}
	} else if errors.As(err, &local) {
		d = local
	}
	return result(struct {
		Error *Diagnostic `json:"error"`
	}{d}, true)
}
