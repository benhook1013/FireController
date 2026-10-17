package mcpadapter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/mcpadapter"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func profiles(t *testing.T, contexts map[string]profile.Context, credential string) string {
	t.Helper()
	entries := map[string]any{}
	for name, c := range contexts {
		entries[name] = map[string]string{"server_url": c.ServerURL, "instance_id": c.InstanceID, "project_id": c.ProjectID, "worker_id": c.WorkerID, "credential_file": credential}
	}
	data, err := json.Marshal(map[string]any{"version": 1, "profiles": entries})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "profiles.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func localContext() profile.Context {
	return profile.Context{Profile: "alpha", ServerURL: "http://127.0.0.1:1", InstanceID: "instance-one", ProjectID: "project-one", WorkerID: "worker-one"}
}
func connect(t *testing.T, server *mcp.Server, version string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	a, b := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, a, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	cs, err := c.Connect(ctx, b, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		ss.Close()
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close(); ss.Close(); cancel() })
	return cs
}
func adapter(t *testing.T, path, name string) *mcp.Server {
	t.Helper()
	s, err := mcpadapter.New([]string{"--profiles-file", path, "--profile", name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func call(t *testing.T, session *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("compatibility result content count=%d", len(result.Content))
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || !json.Valid([]byte(text.Text)) {
		t.Fatal("missing JSON compatibility text")
	}
	structured, _ := json.Marshal(result.StructuredContent)
	var a, b any
	_ = json.Unmarshal(structured, &a)
	_ = json.Unmarshal([]byte(text.Text), &b)
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if !bytes.Equal(aa, bb) {
		t.Fatal("structured/text results disagree")
	}
	return result
}
func decodeResult(t *testing.T, result *mcp.CallToolResult, out any) {
	t.Helper()
	if result.IsError {
		t.Fatalf("tool failed: %v", result.StructuredContent)
	}
	data, _ := json.Marshal(result.StructuredContent)
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatal(err)
	}
}
func errorResult(t *testing.T, result *mcp.CallToolResult, code string, private ...string) {
	t.Helper()
	if !result.IsError {
		t.Fatal("expected tool error")
	}
	data, _ := json.Marshal(result.StructuredContent)
	var v struct {
		Error mcpadapter.Diagnostic `json:"error"`
	}
	if json.Unmarshal(data, &v) != nil || v.Error.Code != code || v.Error.Action == "" {
		t.Fatalf("unexpected diagnostic: %s", data)
	}
	for _, s := range private {
		if s != "" && strings.Contains(string(data), s) {
			t.Fatal("diagnostic exposed private data")
		}
	}
}

// allDiscoveredTools follows the SDK's real discovery cursors without changing
// the production page size or assuming the complete inventory fits one page.
func allDiscoveredTools(t *testing.T, session *mcp.ClientSession) (*mcp.ListToolsResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result := &mcp.ListToolsResult{}
	params := &mcp.ListToolsParams{}
	seenCursors := map[string]bool{}
	seenNames := map[string]bool{}
	for page := 0; page < 8; page++ {
		listed, err := session.ListTools(ctx, params)
		if err != nil {
			return nil, err
		}
		for _, tool := range listed.Tools {
			if seenNames[tool.Name] {
				t.Fatal("duplicate discovered tool", tool.Name)
			}
			seenNames[tool.Name] = true
			result.Tools = append(result.Tools, tool)
		}
		if listed.NextCursor == "" {
			return result, nil
		}
		if len(listed.Tools) == 0 || seenCursors[listed.NextCursor] || listed.NextCursor == params.Cursor {
			t.Fatal("tool discovery cursor did not progress")
		}
		seenCursors[listed.NextCursor] = true
		params.Cursor = listed.NextCursor
	}
	t.Fatal("tool discovery exceeded bounded pages")
	return nil, nil
}

func TestDiscoveryAndLocalContextAcrossSDKProtocolVersions(t *testing.T) {
	c := localContext()
	path := profiles(t, map[string]profile.Context{"alpha": c}, "missing-private-token")
	for _, version := range mcp.SupportedProtocolVersions() {
		t.Run(version, func(t *testing.T) {
			cs := connect(t, adapter(t, path, "alpha"), version)
			if cs.InitializeResult().ProtocolVersion != version {
				t.Fatal("protocol negotiation changed")
			}
			caps := cs.InitializeResult().Capabilities
			if caps.Logging != nil || caps.Resources != nil || caps.Prompts != nil {
				t.Fatal("advertised unimplemented capability")
			}
			first, err := cs.ListTools(context.Background(), nil)
			if err != nil || len(first.Tools) != 64 || first.NextCursor == "" {
				t.Fatal("discovery no longer exercises real pagination", err)
			}
			tools, err := allDiscoveredTools(t, cs)
			if err != nil {
				t.Fatal(err)
			}
			expected := map[string]bool{"context": true, "doctor": true, "workers_list": true, "jobs_list": true, "jobs_assigned": true, "job_read": true, "job_history": true, "inbox_list": true, "inbox_snapshot": true, "inbox_events": true, "inbox_thread": true, "jobs_create": true, "job_edit": true, "inbox_send": true, "inbox_seen": true, "inbox_ack": true}
			expected["job_context"] = true
			expected["wait_for_messages"] = true
			expected["presentation_read"] = true
			for _, name := range []string{"map_entry_create", "map_entry_replace", "map_entry_list", "map_entry_read", "map_entry_history", "map_entry_revision_read", "job_workstream_read", "job_workstream_replace", "job_workstream_history"} {
				expected[name] = true
			}
			expected["note_create"], expected["note_update"], expected["note_read"], expected["note_list"], expected["note_history"], expected["note_revision_read"] = true, true, true, true, true, true
			expected["worker_pause_read"], expected["worker_pause_replace"], expected["worker_pause_history"] = true, true, true
			expected["job_primary_read"], expected["job_primary_replace"], expected["job_primary_history"] = true, true, true
			expected["job_update_append"], expected["job_update_history"], expected["job_checklist_read"], expected["job_checklist_replace"], expected["job_checklist_history"] = true, true, true, true, true
			expected["job_checkpoint_append"], expected["job_checkpoint_latest"], expected["job_checkpoint_history"] = true, true, true
			expected["review_queue"], expected["review_pull"], expected["review_runs"], expected["review_run"], expected["review_attempts"] = true, true, true, true, true
			expected["review_evidence_read"], expected["review_adjudication_read"], expected["review_finding_decide"], expected["review_finding_fix"], expected["review_source_finalize"] = true, true, true, true, true
			expected["review_execution_read"], expected["review_operation_read"], expected["review_request"] = true, true, true
			expected["review_finding_route"], expected["review_route_read"], expected["review_routes_list"], expected["review_route_decide"], expected["review_route_fix"] = true, true, true, true, true
			expected["review_policy_assess"] = true
			expected["review_source_ci_assess"], expected["review_source_readiness"] = true, true
			expected["review_source_refresh"], expected["review_source_refresh_read"], expected["review_source_policy_assess"] = true, true, true
			expected["review_checkpoint_publish"], expected["review_checkpoint_read"], expected["review_checkpoint_recover"] = true, true, true
			writes := map[string]bool{"jobs_create": true, "job_edit": true, "inbox_send": true, "inbox_seen": true, "inbox_ack": true}
			writes["job_primary_replace"] = true
			writes["map_entry_create"], writes["map_entry_replace"], writes["job_workstream_replace"] = true, true, true
			writes["worker_pause_replace"] = true
			writes["note_create"], writes["note_update"] = true, true
			writes["job_update_append"], writes["job_checklist_replace"] = true, true
			writes["job_checkpoint_append"], writes["review_request"], writes["review_source_refresh"] = true, true, true
			writes["review_finding_decide"], writes["review_finding_fix"], writes["review_source_finalize"] = true, true, true
			writes["review_finding_route"], writes["review_route_decide"], writes["review_route_fix"] = true, true, true
			writes["review_checkpoint_publish"], writes["review_checkpoint_recover"] = true, true
			for _, tool := range tools.Tools {
				if !expected[tool.Name] {
					t.Fatalf("unexpected tool %q", tool.Name)
				}
				delete(expected, tool.Name)
				a := tool.Annotations
				if a == nil || a.ReadOnlyHint == writes[tool.Name] || !a.IdempotentHint || a.DestructiveHint == nil || *a.DestructiveHint != (tool.Name == "worker_pause_replace" || tool.Name == "job_primary_replace" || tool.Name == "map_entry_replace" || tool.Name == "job_workstream_replace" || tool.Name == "note_update" || tool.Name == "job_edit" || tool.Name == "job_checklist_replace" || tool.Name == "review_finding_decide" || tool.Name == "review_finding_fix" || tool.Name == "review_finding_route" || tool.Name == "review_route_decide" || tool.Name == "review_route_fix") || a.OpenWorldHint == nil || *a.OpenWorldHint != (tool.Name == "review_request" || tool.Name == "review_source_refresh" || tool.Name == "review_checkpoint_publish" || tool.Name == "review_checkpoint_recover") {
					t.Fatal("untruthful operation annotation")
				}
				data, _ := json.Marshal(tool.InputSchema)
				if strings.Contains(string(data), "profile") || strings.Contains(string(data), "project_id") {
					t.Fatal("per-call binding selector advertised")
				}
			}
			if len(expected) != 0 {
				t.Fatal("missing discovered tools", expected)
			}
			var got profile.Context
			decodeResult(t, call(t, cs, "context", map[string]any{}), &got)
			if got != c {
				t.Fatal("local context changed")
			}
			result := call(t, cs, "doctor", map[string]any{})
			errorResult(t, result, "credential_unreadable", path, "missing-private-token")
		})
	}
}

func TestStrictArgumentsFailBeforeCredentialOrHTTP(t *testing.T) {
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer s.Close()
	c := localContext()
	c.ServerURL = s.URL
	path := profiles(t, map[string]profile.Context{"alpha": c}, "never-open-token")
	cs := connect(t, adapter(t, path, "alpha"), "")
	cases := []struct{ name, raw string }{
		{"doctor", `{"profile":"other"}`}, {"context", `{"project_id":"other"}`}, {"workers_list", `{"limit":1,"limit":2}`}, {"workers_list", `{"Limit":1}`}, {"workers_list", `{"limit":null}`}, {"workers_list", `null`}, {"workers_list", `[]`}, {"workers_list", `{"limit":0}`}, {"workers_list", `{"limit":101}`}, {"workers_list", `{"limit":1.2}`}, {"workers_list", `{"cursor":""}`}, {"jobs_list", `{"worker_id":"../escape"}`}, {"jobs_list", `{"status":"ready"}`}, {"jobs_assigned", `{"worker_id":"other"}`}, {"job_read", `{}`}, {"job_read", `{"job_id":"a/b"}`}, {"job_read", `{"job_id":"bad\ud800"}`}, {"job_history", `{"job_id":"good","cursor":null}`}, {"inbox_list", `{"unread":"true"}`}, {"inbox_list", `{"unread":null}`}, {"inbox_thread", `{"conversation_id":".."}`}, {"inbox_snapshot", `{"unread":"true"}`}, {"inbox_snapshot", `{"cursor":""}`}, {"inbox_snapshot", `{"unacknowledged":null}`}, {"inbox_events", `{}`}, {"inbox_events", `{"cursor":""}`}, {"inbox_events", `{"cursor":null}`}, {"inbox_events", `{"cursor":" padded "}`}, {"inbox_events", `{"cursor":"bad\ncursor"}`}, {"inbox_events", `{"cursor":"cursor","limit":0}`}, {"inbox_events", `{"cursor":"cursor","limit":101}`}, {"inbox_events", `{"cursor":"cursor","wait_ms":-1}`}, {"inbox_events", `{"cursor":"cursor","wait_ms":4001}`}, {"inbox_events", `{"cursor":"cursor","wait_ms":1.2}`}, {"inbox_events", `{"cursor":"cursor","wait_ms":null}`}, {"inbox_events", `{"cursor":"cursor","cursor":"other"}`}, {"workers_list", `{"cursor":"` + strings.Repeat("x", 4097) + `"}`}, {"job_read", `{"job_id":"` + strings.Repeat("x", 129) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name+tc.raw[:min(len(tc.raw), 40)], func(t *testing.T) {
			errorResult(t, call(t, cs, tc.name, json.RawMessage(tc.raw)), "invalid_arguments", path, "never-open-token")
		})
	}
	if calls.Load() != 0 {
		t.Fatal("invalid argument caused network")
	}
	_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "untrusted-secret-name", Arguments: map[string]any{}})
	if err == nil || strings.Contains(err.Error(), "untrusted-secret-name") {
		t.Fatal("unknown tool diagnostic reflected caller input")
	}
}

func TestStartupSelectionAndQuietErrors(t *testing.T) {
	a := localContext()
	b := a
	b.Profile = "beta"
	b.ProjectID = "project-two"
	path := profiles(t, map[string]profile.Context{"alpha": a, "beta": b}, "unopened")
	for _, tc := range []struct {
		args []string
		env  map[string]string
		want profile.Context
	}{
		{[]string{"--profiles-file", path}, map[string]string{"FC_PROFILE": "beta"}, b},
		{[]string{"--profile=alpha", "--profiles-file=" + path}, map[string]string{"FC_PROFILE": "beta"}, a},
	} {
		s, err := mcpadapter.New(tc.args, tc.env)
		if err != nil {
			t.Fatal(err)
		}
		cs := connect(t, s, "")
		var got profile.Context
		decodeResult(t, call(t, cs, "context", nil), &got)
		if got != tc.want {
			t.Fatal("selector precedence changed")
		}
	}
	for _, args := range [][]string{{"--unknown", "private-value"}, {"--profile", "alpha", "--profile", "beta"}, {"--profile"}, {"--profiles-file="}, {"--profile", "alpha", "context"}} {
		var out, stderr bytes.Buffer
		code := mcpadapter.Run(context.Background(), args, nil, strings.NewReader(""), &out, &stderr)
		if code != 2 || out.Len() != 0 || !json.Valid(stderr.Bytes()) || strings.Contains(stderr.String(), "private-value") {
			t.Fatalf("unsafe startup failure code=%d", code)
		}
	}
	_, err := mcpadapter.New(nil, nil)
	var d *mcpadapter.Diagnostic
	if !errors.As(err, &d) || d.Code != "profile_required" {
		t.Fatal("missing identity is not actionable")
	}
	if runtime.GOOS == "linux" {
		base := t.TempDir()
		target := filepath.Join(base, "firecontroller")
		if err = os.MkdirAll(target, 0700); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		if err = os.WriteFile(filepath.Join(target, "profiles.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		s, err := mcpadapter.New(nil, map[string]string{"XDG_CONFIG_HOME": base, "FC_PROFILE": "alpha"})
		if err != nil {
			t.Fatal(err)
		}
		var got profile.Context
		decodeResult(t, call(t, connect(t, s, ""), "context", nil), &got)
		if got != a {
			t.Fatal("default config path")
		}
		_, err = mcpadapter.New(nil, map[string]string{"XDG_CONFIG_HOME": "relative", "FC_PROFILE": "alpha"})
		if !errors.As(err, &d) || d.Code != "config_directory_unavailable" {
			t.Fatal("relative default accepted")
		}
	}
	// Binding is captured, rather than read again on each call.
	cs := connect(t, adapter(t, path, "alpha"), "")
	if err = os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	var got profile.Context
	decodeResult(t, call(t, cs, "context", nil), &got)
	if got != a {
		t.Fatal("startup binding was mutable")
	}
}

func TestRunUsesOnlySDKStdioProtocol(t *testing.T) {
	c := localContext()
	path := profiles(t, map[string]profile.Context{"alpha": c}, "missing")
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- mcpadapter.Run(ctx, []string{"--profile", "alpha", "--profiles-file", path}, nil, inR, outW, &stderr)
		outW.Close()
	}()
	sdk := mcp.NewClient(&mcp.Implementation{Name: "stdio-fixture", Version: "1"}, nil)
	cs, err := sdk.Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got profile.Context
	decodeResult(t, call(t, cs, "context", nil), &got)
	if got != c {
		t.Fatal("stdio context changed")
	}
	cs.Close()
	select {
	case code := <-done:
		if code != 0 || stderr.Len() != 0 {
			t.Fatalf("stdio shutdown code=%d stderr=%s", code, stderr.String())
		}
	case <-ctx.Done():
		t.Fatal("stdio session did not stop within bound")
	}
}
