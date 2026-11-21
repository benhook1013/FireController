package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/strictjson"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func multiProjectUnitSession(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	a, b := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, a, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "catalog-unit-test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, b, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		ss.Close()
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close(); ss.Close(); cancel() })
	return cs
}

func multiProjectUnitCatalog(t *testing.T, cs *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	tools := make(map[string]*mcp.Tool)
	cursor := ""
	for page := 0; page < 8; page++ {
		out, err := cs.ListTools(t.Context(), &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range out.Tools {
			if tools[tool.Name] != nil {
				t.Fatal("duplicate catalog entry", tool.Name)
			}
			tools[tool.Name] = tool
		}
		if out.NextCursor == "" {
			return tools
		}
		if out.NextCursor == cursor || len(out.Tools) == 0 {
			t.Fatal("nonprogressing catalog")
		}
		cursor = out.NextCursor
	}
	t.Fatal("catalog exceeded bounded pages")
	return nil
}

func multiProjectUnitJSON(t *testing.T, value any) []byte {
	t.Helper()
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func multiProjectUnitSchema(t *testing.T, tool *mcp.Tool) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(multiProjectUnitJSON(t, tool.InputSchema), &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestMultiProjectConnectorStaticCatalogPreservesExistingContracts(t *testing.T) {
	singleServer := toolServer(profile.Binding{}, &remoteMCP{})
	multiServer := connectorToolServer(profile.Binding{}, &remoteMCP{}, true)
	singleSession := multiProjectUnitSession(t, singleServer)
	single := multiProjectUnitCatalog(t, singleSession)
	before := multiProjectUnitJSON(t, single)
	multiSession := multiProjectUnitSession(t, multiServer)
	multi := multiProjectUnitCatalog(t, multiSession)
	stdio := multiProjectUnitCatalog(t, multiProjectUnitSession(t, boundServer(profile.Binding{})))
	if !bytes.Equal(before, multiProjectUnitJSON(t, stdio)) {
		t.Fatal("single-project remote catalog differs from stdio")
	}
	if len(multi) != len(single)+1 || single[connectorProjectsTool] != nil {
		t.Fatal("multi-only discovery changed the original tool set")
	}
	for name, original := range single {
		t.Run(name, func(t *testing.T) {
			tool := multi[name]
			if tool == nil || !reflect.DeepEqual(original.Annotations, tool.Annotations) || original.Description != tool.Description {
				t.Fatal("existing description or annotations changed")
			}
			schema := multiProjectUnitSchema(t, tool)
			baseSchema := multiProjectUnitSchema(t, original)
			properties := schema["properties"].(map[string]any)
			project := properties["project"].(map[string]any)
			if project["type"] != "string" || project["minLength"] != float64(1) || project["maxLength"] != float64(128) {
				t.Fatal("project is not a required bounded string")
			}
			if _, exists := project["default"]; exists {
				t.Fatal("project has a default")
			}
			if _, exists := project["enum"]; exists {
				t.Fatal("catalog depends on mutable credential bindings")
			}
			required := schema["required"].([]any)
			if len(required) == 0 || required[len(required)-1] != "project" {
				t.Fatal("missing project requirement")
			}
			// Remove only the new selector and compare every original schema
			// member, including nested constraints and additionalProperties.
			delete(properties, "project")
			if oldRequired, exists := baseSchema["required"]; exists {
				if oldRequired == nil && len(required) != 1 || oldRequired != nil && !reflect.DeepEqual(required[:len(required)-1], oldRequired) {
					t.Fatal("existing required properties changed")
				}
				schema["required"] = oldRequired
			} else {
				delete(schema, "required")
			}
			if !reflect.DeepEqual(schema, baseSchema) || schema["additionalProperties"] != false {
				t.Fatal("existing schema contract changed")
			}
		})
	}
	projects := multi[connectorProjectsTool]
	if projects == nil || projects.Annotations == nil || !projects.Annotations.ReadOnlyHint || !projects.Annotations.IdempotentHint || projects.Annotations.DestructiveHint == nil || *projects.Annotations.DestructiveHint || projects.Annotations.OpenWorldHint == nil || *projects.Annotations.OpenWorldHint {
		t.Fatal("project catalog annotations are not read-only and closed-world")
	}
	projectSchema := multiProjectUnitSchema(t, projects)
	if len(projectSchema["properties"].(map[string]any)) != 0 || projectSchema["additionalProperties"] != false {
		t.Fatal("project catalog accepts arguments")
	}
	// Stable SDK cursors must remain usable across stateless connections to the
	// same prebuilt catalog, without adding/removing tools per credential call.
	page, err := multiSession.ListTools(t.Context(), nil)
	if err != nil || page.NextCursor == "" {
		t.Fatal("expected a paginated multi-project catalog", err)
	}
	other := multiProjectUnitSession(t, multiServer)
	a, err := multiSession.ListTools(t.Context(), &mcp.ListToolsParams{Cursor: page.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	b, err := other.ListTools(t.Context(), &mcp.ListToolsParams{Cursor: page.NextCursor})
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("prebuilt catalog pagination changed between connections", err)
	}
	if !bytes.Equal(before, multiProjectUnitJSON(t, multiProjectUnitCatalog(t, singleSession))) {
		t.Fatal("building or reading the multi catalog mutated the single catalog")
	}
}

func TestMultiProjectConnectorSchemaCopiesOuterState(t *testing.T) {
	required := make([]string, 1, 8)
	required[0] = "document"
	original := definition{"example", "description", map[string]any{
		"type": "object", "$id": "retained-schema-id", "additionalProperties": false,
		"required": required, "properties": map[string]any{"document": map[string]any{"type": "object"}},
	}}
	before := multiProjectUnitJSON(t, original.schema)
	converted := multiProjectDefinition(original)
	if !bytes.Equal(before, multiProjectUnitJSON(t, original.schema)) {
		t.Fatal("conversion mutated original schema")
	}
	converted.schema["$id"] = "different"
	converted.schema["required"].([]string)[0] = "different"
	converted.schema["properties"].(map[string]any)["another"] = true
	if !bytes.Equal(before, multiProjectUnitJSON(t, original.schema)) {
		t.Fatal("converted outer schema, required list or properties alias the original")
	}
}

func TestMultiProjectConnectorArgumentsRemoveOnlySelector(t *testing.T) {
	cases := []struct{ raw, cleaned string }{
		{`{"project":"alpha"}`, `{}`},
		{`{"project":"alpha","limit":2}`, `{"limit":2}`},
		{`{"limit":2,"project":"alpha"}`, `{"limit":2}`},
		{`{"limit":2,"project":"alpha","status":"open"}`, `{"limit":2,"status":"open"}`},
		{" \n{ \t\"project\" : \"alpha\" \n, \"limit\":2 } \t", " \n{ \t \"limit\":2 } \t"},
		{"{\"limit\":2 , \"project\" : \"alpha\" \n }", "{\"limit\":2  \n }"},
		{`{"pro\u006aect":"alpha","document": { "project":"nested", "n": 1e2, "text":"\u0061" }}`, `{"document": { "project":"nested", "n": 1e2, "text":"\u0061" }}`},
	}
	for _, tt := range cases {
		t.Run(tt.raw, func(t *testing.T) {
			raw := []byte(tt.raw)
			before := slices.Clone(raw)
			cleaned, project, err := selectedConnectorProjectArguments(raw)
			if err != nil || project != "alpha" || strictjson.Validate(cleaned) != nil || string(cleaned) != tt.cleaned {
				t.Fatalf("selected=%q cleaned=%q err=%v, want %q", project, cleaned, err, tt.cleaned)
			}
			if !bytes.Equal(raw, before) {
				t.Fatal("selector removal changed the original arguments")
			}
		})
	}
	// Keep the original parser's encoded-byte limit, even for whitespace.
	raw := `{"project":"alpha",` + strings.Repeat(" ", 16<<10) + `"limit":2}`
	cleaned, _, err := selectedConnectorProjectArguments([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseArguments("jobs_list", cleaned); err == nil {
		t.Fatal("selector removal weakened the original argument byte budget")
	}
	for _, raw := range []string{
		`{"project":"alpha","limit":null}`, `{"project":"alpha","Limit":2}`,
		`{"project":"alpha","profile":"another"}`, `{"project":"alpha","worker_id":null}`,
	} {
		cleaned, _, err := selectedConnectorProjectArguments([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseArguments("jobs_list", cleaned); err == nil {
			t.Fatalf("existing parser accepted invalid remaining fields: %s", raw)
		}
	}
}

func TestMultiProjectConnectorArgumentsRejectBeforeDispatch(t *testing.T) {
	cases := []string{
		"", `{}`, `null`, `[]`, `"alpha"`, `{"limit":2}`, `{"project":null}`,
		`{"project":3}`, `{"project":true}`, `{"project":[]}`, `{"project":{}}`,
		`{"project":""}`, `{"project":" alpha"}`, `{"project":"alpha "}`, `{"project":"alpha\n"}`,
		`{"Project":"alpha"}`, `{"PROJECT":"alpha"}`, `{"project":"alpha","Project":"alpha"}`,
		`{"project":"alpha","project":"beta"}`, `{"project":"alpha","pro\u006aect":"beta"}`,
		`{"project":"alpha","limit":1,"limit":2}`,
		`{"project":"alpha","document":{"value":1,"value":2}}`,
		`{"project":"alpha","document":{"value":"\ud800"}}`,
		`{"project":"alpha"} {}`, `{"project":"alpha",}`, `{"project":"` + strings.Repeat("x", 129) + `"}`,
		`{"project":"alpha","body":"` + strings.Repeat("x", remoteBodyBytes) + `"}`,
		"{\"project\":\"alpha\",\"body\":\"\xff\"}",
	}
	for index, raw := range cases {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			cleaned, project, err := selectedConnectorProjectArguments([]byte(raw))
			var diagnostic *Diagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Code != "invalid_arguments" || cleaned != nil || project != "" {
				t.Fatal("invalid selector or non-strict JSON survived selection")
			}
		})
	}
}

type multiProjectUnitStore struct {
	mu         sync.Mutex
	handles    map[string]string
	active     map[string]string // Exact project-to-worker binding, not a project allowlist.
	lookups    []string
	checks     []core.ConnectorBinding
	onLookup   func(string)
	handleFail bool
}

func (s *multiProjectUnitStore) ProjectHandle(ctx context.Context, project string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookups = append(s.lookups, project)
	if s.onLookup != nil {
		s.onLookup(project)
	}
	if s.handleFail {
		return "", errors.New("metadata unavailable")
	}
	return s.handles[project], ctx.Err()
}

func (s *multiProjectUnitStore) ReauthorizeConnector(ctx context.Context, token string, binding core.ConnectorBinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks = append(s.checks, binding)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if token != "unit-credential" || s.active[binding.ProjectID] != binding.WorkerID || binding.WorkerID == "" {
		return errors.New("inactive binding")
	}
	return nil
}

func multiProjectUnitAccess() (*connectorRequest, *multiProjectUnitStore) {
	return &connectorRequest{
		identity: profile.Context{Profile: "connector", ServerURL: "https://unit.example.test", InstanceID: "unit-instance"},
		access: core.ConnectorAccess{MultiProject: true, Bindings: []core.ConnectorBinding{
			{ProjectID: "project-one", WorkerID: "worker-one"}, {ProjectID: "project-two", WorkerID: "worker-two"},
		}}, token: "unit-credential", lifetime: context.Background(),
	}, &multiProjectUnitStore{
		handles: map[string]string{"project-one": "alpha", "project-two": "beta", "foreign-project": "foreign"},
		active:  map[string]string{"project-one": "worker-one", "project-two": "worker-two"},
	}
}

func TestMultiProjectConnectorSelectsExactActiveBindingWithoutSharedState(t *testing.T) {
	request, store := multiProjectUnitAccess()
	before := *request
	for _, tt := range []struct{ selector, project, worker string }{
		{"alpha", "project-one", "worker-one"}, {"beta", "project-two", "worker-two"},
		{"project-one", "project-one", "worker-one"}, {"project-two", "project-two", "worker-two"},
	} {
		t.Run(tt.selector, func(t *testing.T) {
			selected, err := selectConnectorProject(t.Context(), store, request, tt.selector)
			if err != nil || selected == request || selected.identity.ProjectID != tt.project || selected.identity.WorkerID != tt.worker {
				t.Fatalf("incorrect explicit binding: %+v, %v", selected, err)
			}
			if selected.binding.ProjectID != tt.project || selected.binding.WorkerID != tt.worker || selected.token != request.token || selected.lifetime != request.lifetime {
				t.Fatal("dispatch lost its selected reauthorization/replay binding")
			}
			if selected.identity.ServerURL != request.identity.ServerURL || selected.identity.InstanceID != request.identity.InstanceID {
				t.Fatal("project selector changed repository connection identity")
			}
		})
	}
	for _, selector := range []string{"", "Alpha", "BETA", " alpha", "alpha ", "foreign", "foreign-project", "public-slug", "worker-one"} {
		if _, err := selectConnectorProject(t.Context(), store, request, selector); err == nil {
			t.Fatalf("noncanonical or unauthorized selector accepted: %q", selector)
		}
	}
	for _, project := range store.lookups {
		if project != "project-one" && project != "project-two" {
			t.Fatal("looked up metadata outside current credential bindings")
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			project, worker := "project-one", "worker-one"
			if i%2 != 0 {
				project, worker = "project-two", "worker-two"
			}
			selected, err := selectConnectorProject(t.Context(), store, request, project)
			if err != nil || selected.identity.ProjectID != project || selected.identity.WorkerID != worker {
				t.Error("parallel call switched project or worker", err)
			}
		}(i)
	}
	wg.Wait()
	if !reflect.DeepEqual(*request, before) {
		t.Fatal("selection mutated shared request identity or access")
	}
	// Even with several authorized projects there is never an implicit default;
	// the immutable credential mode, not active-binding count, selects the shape.
	request.access.Bindings = request.access.Bindings[:1]
	if _, err := selectConnectorProject(t.Context(), store, request, ""); err == nil {
		t.Fatal("one remaining active binding acquired a default")
	}
	request.access.MultiProject = false
	if _, err := selectConnectorProject(t.Context(), store, request, "alpha"); err == nil {
		t.Fatal("single-project connector accepted a selector")
	}
}

func TestMultiProjectConnectorProjectCatalogIsMinimalAndRevalidated(t *testing.T) {
	request, store := multiProjectUnitAccess()
	out, err := connectorProjectCatalog(t.Context(), store, request)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"projects":[{"project_id":"project-one","project_handle":"alpha","worker_id":"worker-one"},{"project_id":"project-two","project_handle":"beta","worker_id":"worker-two"}]}`
	if string(multiProjectUnitJSON(t, out)) != want {
		t.Fatalf("catalog exposed fields beyond active identities or mismatched workers: %+v", out)
	}
	if len(store.checks) != 4 {
		t.Fatal("catalog did not validate before lookup and recheck before delivery")
	}
	delete(store.active, "project-two")
	store.lookups = nil
	out, err = connectorProjectCatalog(t.Context(), store, request)
	if err == nil || len(out.Projects) != 0 || slices.Contains(store.lookups, "project-two") {
		t.Fatal("inactive binding reached a metadata lookup or partial catalog result")
	}
	if _, err := selectConnectorProject(t.Context(), store, request, "project-two"); err == nil {
		t.Fatal("ID selection bypassed active credential validation")
	}
	store.active["project-two"] = "other-worker"
	if _, err := selectConnectorProject(t.Context(), store, request, "project-two"); err == nil {
		t.Fatal("project-only authorization accepted the wrong acting worker")
	}
	store.active["project-two"] = "worker-two"
	store.onLookup = func(project string) { delete(store.active, "project-one") }
	out, err = connectorProjectCatalog(t.Context(), store, request)
	if err == nil || len(out.Projects) != 0 {
		t.Fatal("revocation during lookup leaked the catalog")
	}
	request.access.MultiProject = false
	if _, err := connectorProjectCatalog(t.Context(), store, request); err == nil {
		t.Fatal("single-project connector exposed the multi-project lookup")
	}
}

func TestMultiProjectConnectorProjectHandleFallbackAndCancellation(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			request, store := multiProjectUnitAccess()
			store.handles = nil
			store.handleFail = failed
			out, err := connectorProjectCatalog(t.Context(), store, request)
			if err != nil || len(out.Projects) != 2 {
				t.Fatal("authorized opaque IDs require friendly metadata", err)
			}
			for _, project := range out.Projects {
				if project.ProjectHandle != project.ProjectID {
					t.Fatal("missing canonical handle did not fall back to ID")
				}
				if _, err := selectConnectorProject(t.Context(), store, request, project.ProjectID); err != nil {
					t.Fatal("fallback project ID cannot be selected", err)
				}
			}
		})
	}
	request, store := multiProjectUnitAccess()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := connectorProjectCatalog(ctx, store, request); err == nil || len(store.lookups) != 0 {
		t.Fatal("canceled credential validation reached catalog metadata")
	}
	if _, err := selectConnectorProject(ctx, store, request, "project-one"); err == nil {
		t.Fatal("canceled credential validation selected a binding")
	}
}

func TestMultiProjectConnectorUncertainReplayRetainsProject(t *testing.T) {
	service := &client.Error{Code: "outcome_unknown", Message: "unchanged", Action: "Recover with original key.", OutcomeUnknown: true}
	local := connectorAccessChanged(true)
	for _, original := range []error{service, local} {
		before := multiProjectUnitJSON(t, original)
		updated := connectorProjectReplayError(original)
		if updated == original || !bytes.Equal(before, multiProjectUnitJSON(t, original)) {
			t.Fatal("project replay guidance mutated the original diagnostic")
		}
		out := resultFailure(updated)
		var value struct {
			Error Diagnostic `json:"error"`
		}
		if json.Unmarshal(multiProjectUnitJSON(t, out.StructuredContent), &value) != nil || !value.Error.OutcomeUnknown || !strings.Contains(value.Error.Action, "original project selector") {
			t.Fatal("uncertain write lost project-specific recovery guidance")
		}
	}
	read := connectorAccessChanged(false)
	if connectorProjectReplayError(read) != read || connectorProjectReplayError(nil) != nil {
		t.Fatal("project recovery changed a known read failure")
	}
}
