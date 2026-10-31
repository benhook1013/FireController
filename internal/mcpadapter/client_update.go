package mcpadapter

import (
	"encoding/json"
	"sync"

	"github.com/benhook1013/FireController/internal/buildinfo"
	"github.com/benhook1013/FireController/internal/strictjson"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type clientUpdateNotice struct {
	mu         sync.Mutex
	lastTarget string
	running    buildinfo.Client
	target     func() (buildinfo.Source, bool)
}

func newClientUpdateNotice() *clientUpdateNotice {
	return &clientUpdateNotice{running: buildinfo.Current(Version), target: buildinfo.LaunchTarget}
}

// Apply adds one local connection hint, never a new result envelope or source
// fact. Unknown installation metadata cannot fail the original operation.
func (n *clientUpdateNotice) Apply(response *mcp.CallToolResult) {
	if response == nil || n.running.SourceRevision == "unknown" || n.running.SourceRevision == "" {
		return
	}
	target, ok := n.target()
	if !ok || target.Revision == "" || target.Revision == "unknown" {
		return
	}
	if target.Revision == n.running.SourceRevision && (target.State == n.running.SourceState || target.State == "unknown" || n.running.SourceState == "unknown") {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.lastTarget == target.Revision {
		return
	}
	data, err := json.Marshal(response.StructuredContent)
	if err != nil || len(data) > 4<<20 || len(response.Content) != 1 || strictjson.Validate(data) != nil {
		return
	}
	text, ok := response.Content[0].(*mcp.TextContent)
	if !ok || text.Text != string(data) {
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil || fields["client_update"] != nil {
		return
	}
	notice, err := json.Marshal(struct {
		RunningRevision string `json:"running_source_revision"`
		RunningState    string `json:"running_source_state"`
		LaunchRevision  string `json:"launch_target_source_revision"`
		LaunchState     string `json:"launch_target_source_state"`
		NextAction      string `json:"next_action"`
		Message         string `json:"message"`
	}{n.running.SourceRevision, n.running.SourceState, target.Revision, target.State, "reconnect_mcp", "This MCP process differs from its installed launch target. Reconnect this MCP server through its host using the same private binding. Installing a client does not replace an existing process."})
	if err != nil {
		return
	}
	fields["client_update"] = notice
	updated, err := json.Marshal(fields)
	if err != nil || len(updated) > 4<<20 {
		return
	}
	copy := *text
	copy.Text = string(updated)
	response.StructuredContent = fields
	response.Content = []mcp.Content{&copy}
	n.lastTarget = target.Revision
}
