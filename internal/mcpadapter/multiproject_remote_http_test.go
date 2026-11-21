package mcpadapter_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRemoteMultiProjectConnectorSDKScopesDiscoveryAndReplay(t *testing.T) {
	f, service, waitReady, waitDone, _ := connectorFixture(t)
	second := connectorNative(t, f, "POST", "/api/v1/projects", f.admin, "", "multi-second-project",
		map[string]string{"name": "Second project", "repository": "example/second", "handle": "second-project"}, 200)["id"].(string)
	secondWorker := connectorNative(t, f, "POST", "/api/v1/projects/"+second+"/workers", f.admin, "", "multi-second-worker",
		map[string]string{"name": "Second worker"}, 200)["id"].(string)
	third := connectorNative(t, f, "POST", "/api/v1/projects", f.admin, "", "multi-third-project",
		map[string]string{"name": "Third project", "repository": "example/third"}, 200)["id"].(string)
	bearer := core.NewID("synthetic")
	hash := sha256.Sum256([]byte(bearer))
	projects := []map[string]any{
		{"project_id": f.project, "worker_id": f.sender, "permissions": []string{"read", "write"}},
		{"project_id": second, "worker_id": secondWorker, "permissions": []string{"read", "write"}},
	}
	receipt := connectorNative(t, f, "POST", "/api/v1/operator/connector-credentials", f.admin, "", "multi-grant",
		map[string]any{"token_sha256": hex.EncodeToString(hash[:]), "projects": projects}, 200)

	singleBearer := core.NewID("synthetic")
	connectorGrant(t, f, singleBearer, f.sender, "legacy-connector", []string{"read"}, 200)
	single := connectorSDK(t, service, singleBearer, "2025-11-25")
	multi := connectorSDK(t, service, bearer, "2025-11-25")
	legacyCatalog := remoteToolCatalog(t, single)
	multiCatalog := remoteToolCatalog(t, multi)
	if len(multiCatalog) != len(legacyCatalog)+1 {
		t.Fatalf("scope-aware catalog size %d, legacy %d", len(multiCatalog), len(legacyCatalog))
	}
	legacyTools := make(map[string]*mcp.Tool, len(legacyCatalog))
	for _, tool := range legacyCatalog {
		legacyTools[tool.Name] = tool
	}
	for _, tool := range multiCatalog {
		legacy := legacyTools[tool.Name]
		if tool.Name == "connector_projects" {
			if legacy != nil || tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Fatal("project discovery should be read-only and multi-project only")
			}
			continue
		}
		if legacy == nil || !reflect.DeepEqual(tool.Annotations, legacy.Annotations) {
			t.Fatalf("missing legacy tool or changed annotations for %s", tool.Name)
		}
		schema, ok := tool.InputSchema.(map[string]any)
		if !ok {
			t.Fatalf("%s has no object schema", tool.Name)
		}
		properties := schema["properties"].(map[string]any)
		selector, ok := properties["project"].(map[string]any)
		if !ok || selector["type"] != "string" || selector["default"] != nil {
			t.Fatalf("%s has an invalid project selector", tool.Name)
		}
		required := schema["required"].([]any)
		if !containsProjectSelector(required) || schema["additionalProperties"] != false {
			t.Fatalf("%s schema does not require project or allowlist properties", tool.Name)
		}
	}
	list := call(t, multi, "connector_projects", map[string]any{})
	if list.IsError {
		t.Fatal("granted project discovery failed")
	}
	encoded, err := json.Marshal(list.StructuredContent)
	if err != nil || !strings.Contains(string(encoded), f.project) || !strings.Contains(string(encoded), second) ||
		!strings.Contains(string(encoded), "second-project") || strings.Contains(string(encoded), third) {
		t.Fatal("discovery did not return exactly the allowed project identities", err)
	}

	for _, input := range []any{map[string]any{}, map[string]any{"project": third}, map[string]any{"project": nil}} {
		out, err := multi.CallTool(t.Context(), &mcp.CallToolParams{Name: "context", Arguments: input})
		if err == nil && (out == nil || !out.IsError) {
			t.Fatal("accepted missing, malformed or foreign project selector")
		}
	}
	targets := []struct{ selector, projectID, workerID string }{
		{f.project, f.project, f.sender},
		{"second-project", second, secondWorker},
	}
	jobArgs := func(project string) map[string]any {
		return map[string]any{"project": project, "title": "Synthetic selected project", "status": "completed", "idempotency_key": "same-key-for-two-projects"}
	}
	var results [2]client.JobReceipt
	for _, target := range targets {
		var identity profile.Context
		decodeResult(t, call(t, multi, "context", map[string]any{"project": target.selector}), &identity)
		if identity.ProjectID != target.projectID || identity.WorkerID != target.workerID {
			t.Fatal("selected project did not use its retained worker")
		}
	}
	type creation struct {
		index int
		out   *mcp.CallToolResult
		err   error
	}
	created := make(chan creation, len(targets))
	for i, target := range targets {
		go func() {
			out, err := multi.CallTool(t.Context(), &mcp.CallToolParams{Name: "jobs_create", Arguments: jobArgs(target.selector)})
			created <- creation{i, out, err}
		}()
	}
	for range targets {
		value := <-created
		if value.err != nil {
			t.Fatal(value.err)
		}
		decodeResult(t, value.out, &results[value.index])
	}
	if results[0].ID == "" || results[1].ID == "" || results[0].ID == results[1].ID {
		t.Fatal("same request key crossed project boundaries")
	}
	for i, target := range targets {
		var replay client.JobReceipt
		decodeResult(t, call(t, multi, "jobs_create", jobArgs(target.selector)), &replay)
		if !reflect.DeepEqual(replay, results[i]) {
			t.Fatal("exact same-project replay changed receipt")
		}
		var count int
		if err := f.store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type='job.created' AND project_id=? AND resource_id=? AND actor_id=?`,
			target.projectID, replay.ID, receipt["principal_id"]).Scan(&count); err != nil || count != 1 {
			t.Fatal("write event was duplicated or attributed to the wrong project or principal", count, err)
		}
	}
	if foreign := call(t, multi, "job_read", map[string]any{"project": f.project, "job_id": results[1].ID}); !foreign.IsError {
		t.Fatal("job read crossed projects")
	}

	var snapshot client.InboxSnapshot
	decodeResult(t, call(t, multi, "inbox_snapshot", map[string]any{"project": f.project}), &snapshot)
	type waitOutcome struct {
		result *mcp.CallToolResult
		err    error
	}
	waiting := make(chan waitOutcome, 1)
	go func() {
		out, err := multi.CallTool(t.Context(), &mcp.CallToolParams{Name: "wait_for_messages", Arguments: map[string]any{
			"project": f.project, "cursor": snapshot.EventCursor, "wait_seconds": 5,
		}})
		waiting <- waitOutcome{out, err}
	}()
	waitConnectorSignal(t, waitReady)
	projectPath := "/api/v1/projects/" + f.project
	grantPath := projectPath + "/credentials/" + receipt["id"].(string)
	connectorNative(t, f, "PUT", grantPath, f.admin, "", "withdraw-selected-write",
		map[string]any{"expected_permissions": []string{"read", "write"}, "permissions": []string{"read"}}, 200)
	connectorNative(t, f, "POST", projectPath+"/messages", f.admin, f.recipient, "deliver-after-selected-downgrade",
		map[string]string{"recipient_worker_id": f.sender, "body": "Synthetic selected-project wait result"}, 201)
	select {
	case value := <-waiting:
		if value.err != nil {
			t.Fatal(value.err)
		}
		errorResult(t, value.result, "connector_access_changed", bearer, "Synthetic selected-project wait result")
	case <-time.After(4 * time.Second):
		t.Fatal("selected-project wait did not settle after downgrade")
	}
	waitConnectorSignal(t, waitDone)
	connectorNative(t, f, "DELETE", grantPath, f.admin, "", "withdraw-one-project", nil, 200)
	if denied := call(t, multi, "context", map[string]any{"project": f.project}); !denied.IsError {
		t.Fatal("withdrawn binding remained usable")
	}
	var remaining profile.Context
	decodeResult(t, call(t, multi, "context", map[string]any{"project": second}), &remaining)
	if remaining.ProjectID != second || remaining.WorkerID != secondWorker {
		t.Fatal("withdrawing one grant changed the other binding")
	}
	// The transport can reject a malformed envelope before MCP dispatch. Use a
	// separate SDK connection because this rejection may close the client session.
	malformed := connectorSDK(t, service, bearer, "2025-11-25")
	duplicate := json.RawMessage(`{"project":"second-project","project":"second-project"}`)
	out, err := malformed.CallTool(t.Context(), &mcp.CallToolParams{Name: "context", Arguments: duplicate})
	if err == nil && (out == nil || !out.IsError) {
		t.Fatal("accepted duplicate project selectors")
	}
}

func containsProjectSelector(values []any) bool {
	for _, value := range values {
		if value == "project" {
			return true
		}
	}
	return false
}
