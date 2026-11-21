package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const connectorProjectsTool = "connector_projects"

// Only the outer object changes. Nested schemas and all existing field contracts
// remain shared, read-only definitions; neither catalog can mutate the other.
func multiProjectDefinition(tool definition) definition {
	schema := maps.Clone(tool.schema)
	properties, _ := schema["properties"].(map[string]any)
	properties = maps.Clone(properties)
	if properties == nil {
		properties = make(map[string]any)
	}
	properties["project"] = map[string]any{
		"type": "string", "minLength": 1, "maxLength": 128,
		"description": "Required exact canonical project handle or project ID from connector_projects. No default. The selected project's bound worker acts on this call. Preserve this project, the exact arguments and key when recovering an uncertain write.",
	}
	required, _ := schema["required"].([]string)
	schema["required"] = append(append([]string(nil), required...), "project")
	schema["properties"] = properties
	tool.schema = schema
	return tool
}

func connectorProjectsDefinition() definition {
	return definition{connectorProjectsTool, "List only this connector credential's currently active project IDs, canonical handles and matching acting worker IDs. Read-only; takes no arguments. Select an exact project handle or ID explicitly on every other tool call.", map[string]any{
		"type": "object", "properties": map[string]any{}, "additionalProperties": false,
	}}
}

// Validate the original bytes before removing anything, including duplicates in
// nested documents. Retain all remaining bytes rather than re-encoding a map:
// existing parsers still enforce their own bounds, exact fields and null rules.
func selectedConnectorProjectArguments(raw json.RawMessage) (json.RawMessage, string, error) {
	if len(raw) > remoteBodyBytes || strictjson.Validate(raw) != nil {
		return nil, "", invalidArguments()
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, "", invalidArguments()
	}
	var project string
	var start, end int
	first, found := false, false
	for index := 0; decoder.More(); index++ {
		memberStart := int(decoder.InputOffset())
		key, _ := decoder.Token()
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, "", invalidArguments()
		}
		name, _ := key.(string)
		if strings.EqualFold(name, "project") {
			if name != "project" || json.Unmarshal(value, &project) != nil || !validID(project) {
				return nil, "", invalidArguments()
			}
			start, end = memberStart, int(decoder.InputOffset())
			first, found = index == 0, true
		}
	}
	if !found {
		return nil, "", invalidArguments()
	}
	if first {
		// A first member owns the following comma, other members own the
		// preceding comma. Whitespace outside that member stays untouched.
		for next := end; next < len(raw); next++ {
			if strings.ContainsRune(" \t\r\n", rune(raw[next])) {
				continue
			}
			if raw[next] == ',' {
				end = next + 1
			}
			break
		}
	}
	cleaned := make(json.RawMessage, 0, len(raw)-(end-start))
	cleaned = append(cleaned, raw[:start]...)
	cleaned = append(cleaned, raw[end:]...)
	return cleaned, project, nil
}

// Project metadata lookups require the same current binding validation as tools.
type connectorProjectStore interface {
	ProjectHandle(context.Context, string) (string, error)
	ReauthorizeConnector(context.Context, string, core.ConnectorBinding) error
}

func reauthorizeConnector(ctx context.Context, store connectorProjectStore, token string, binding core.ConnectorBinding) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return store.ReauthorizeConnector(ctx, token, binding)
}

func connectorProjectHandle(ctx context.Context, store connectorProjectStore, token string, binding core.ConnectorBinding) (string, error) {
	if err := reauthorizeConnector(ctx, store, token, binding); err != nil {
		return "", connectorAccessChanged(false)
	}
	handle, err := store.ProjectHandle(ctx, binding.ProjectID)
	if err != nil || handle == "" {
		// Handles are friendly selectors, never authority. Missing/unavailable
		// metadata must not prevent selection by the authorized opaque ID.
		handle = binding.ProjectID
	}
	return handle, nil
}

func selectConnectorProject(ctx context.Context, store connectorProjectStore, request *connectorRequest, project string) (*connectorRequest, error) {
	if !request.access.MultiProject || project == "" {
		return nil, invalidArguments()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var selected *core.ConnectorBinding
	// Opaque IDs need no metadata lookup and never use a global resolver.
	for _, binding := range request.access.Bindings {
		if project == binding.ProjectID {
			if reauthorizeConnector(ctx, store, request.token, binding) != nil {
				return nil, connectorAccessChanged(false)
			}
			selected = &binding
			break
		}
	}
	if selected == nil {
		for _, binding := range request.access.Bindings {
			handle, err := connectorProjectHandle(ctx, store, request.token, binding)
			if err != nil {
				return nil, err
			}
			if project == handle {
				selected = &binding
				break
			}
		}
	}
	if selected == nil {
		return nil, diagnostic("invalid_arguments", "Select an active project from this connector's project catalog.", "Call connector_projects and use an exact project handle or project ID. No request was sent.")
	}
	// Copy the request as well as its identity: parallel calls must never change
	// each other's project/worker, permission snapshot or uncertain-write replay.
	call := *request
	call.binding = *selected
	call.identity.ProjectID = selected.ProjectID
	call.identity.WorkerID = selected.WorkerID
	return &call, nil
}

type connectorProject struct {
	ProjectID     string `json:"project_id"`
	ProjectHandle string `json:"project_handle"`
	WorkerID      string `json:"worker_id"`
}

type connectorProjects struct {
	Projects []connectorProject `json:"projects"`
}

func connectorProjectCatalog(ctx context.Context, store connectorProjectStore, request *connectorRequest) (connectorProjects, error) {
	if !request.access.MultiProject || len(request.access.Bindings) == 0 {
		return connectorProjects{}, connectorAccessChanged(false)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	value := connectorProjects{Projects: make([]connectorProject, 0, len(request.access.Bindings))}
	for _, binding := range request.access.Bindings {
		handle, err := connectorProjectHandle(ctx, store, request.token, binding)
		if err != nil {
			return connectorProjects{}, err
		}
		value.Projects = append(value.Projects, connectorProject{binding.ProjectID, handle, binding.WorkerID})
	}
	// A revocation or downgrade during a handle lookup must suppress the result,
	// just as the ordinary dispatch path suppresses sensitive wait results.
	for _, binding := range request.access.Bindings {
		if reauthorizeConnector(ctx, store, request.token, binding) != nil {
			return connectorProjects{}, connectorAccessChanged(false)
		}
	}
	return value, nil
}

// An uncertain write retains its project as well as its original key and body.
// Copy diagnostics so adding routing guidance cannot alter a shared error.
func connectorProjectReplayError(err error) error {
	const guidance = " Retain the original project selector; never recover this write in another project."
	var service *client.Error
	if errors.As(err, &service) && service.OutcomeUnknown {
		copy := *service
		copy.Action += guidance
		return &copy
	}
	var local *Diagnostic
	if errors.As(err, &local) && local.OutcomeUnknown {
		copy := *local
		copy.Action += guidance
		return &copy
	}
	return err
}
