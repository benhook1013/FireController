package mcpadapter

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/benhook1013/FireController/internal/buildinfo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func updateFixture(target *buildinfo.Source, available *bool) *clientUpdateNotice {
	return &clientUpdateNotice{running: buildinfo.Client{Version: Version, SourceRevision: strings.Repeat("a", 40), SourceState: "clean"}, target: func() (buildinfo.Source, bool) { return *target, *available }}
}

func updateResult(v any) *mcp.CallToolResult {
	data, _ := json.Marshal(v)
	return &mcp.CallToolResult{StructuredContent: v, Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}
}

func TestClientUpdateNoticePreservesResultsAndWarnsOncePerTarget(t *testing.T) {
	target := buildinfo.Source{Revision: strings.Repeat("b", 40), State: "clean"}
	available := true
	n := updateFixture(&target, &available)
	original := json.RawMessage(`{"operation":{"id":"original","head_sha":"unchanged"},"items":[1,2],"state":"pending"}`)
	first := updateResult(original)
	first.IsError = true
	n.Apply(first)
	encoded, _ := json.Marshal(first.StructuredContent)
	if first.Content[0].(*mcp.TextContent).Text != string(encoded) || !first.IsError {
		t.Fatal("text/structured/error representation changed")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(encoded, &fields) != nil || len(fields) != 4 || fields["client_update"] == nil {
		t.Fatal("local update metadata missing", string(encoded))
	}
	delete(fields, "client_update")
	var prior map[string]json.RawMessage
	json.Unmarshal(original, &prior)
	if !reflect.DeepEqual(fields, prior) {
		t.Fatal("update notice overwrote source facts")
	}
	for i := 0; i < 2; i++ {
		next := updateResult(original)
		before, _ := json.Marshal(next)
		n.Apply(next)
		after, _ := json.Marshal(next)
		if string(before) != string(after) {
			t.Fatal("repeated hint changed original result")
		}
	}
	target.Revision = strings.Repeat("c", 40)
	changed := updateResult(original)
	n.Apply(changed)
	if !strings.Contains(changed.Content[0].(*mcp.TextContent).Text, target.Revision) {
		t.Fatal("changed launch target did not warn again")
	}
}

func TestClientUpdateNoticeUnknownAndUnsafeResultsStayUnchanged(t *testing.T) {
	target := buildinfo.Source{Revision: strings.Repeat("b", 40), State: "clean"}
	available := true
	for name, value := range map[string]any{"array": []int{1}, "null": nil, "owned metadata": map[string]any{"client_update": nil, "source": "original"}, "duplicate keys": json.RawMessage(`{"state":"old","state":"new"}`), "too large after notice": map[string]any{"source": strings.Repeat("x", (4<<20)-32)}} {
		t.Run(name, func(t *testing.T) {
			n := updateFixture(&target, &available)
			r := updateResult(value)
			before, _ := json.Marshal(r)
			n.Apply(r)
			after, _ := json.Marshal(r)
			if string(before) != string(after) || n.lastTarget != "" {
				t.Fatal("unsafe result changed or consumed notification")
			}
			safe := updateResult(map[string]string{"state": "pending"})
			n.Apply(safe)
			if n.lastTarget != target.Revision {
				t.Fatal("skipped result suppressed later valid notice")
			}
		})
	}
	for _, mode := range []string{"unavailable", "matching", "unknown running"} {
		n := updateFixture(&target, &available)
		switch mode {
		case "unavailable":
			n.target = func() (buildinfo.Source, bool) { return buildinfo.Source{}, false }
		case "matching":
			n.running.SourceRevision = target.Revision
		case "unknown running":
			n.running.SourceRevision = "unknown"
		}
		r := updateResult(map[string]string{"state": "pending"})
		before, _ := json.Marshal(r)
		n.Apply(r)
		after, _ := json.Marshal(r)
		if string(before) != string(after) {
			t.Fatal("unknown or matching target emitted warning", mode)
		}
	}
}

func TestClientUpdateNoticeConcurrentResultsWarnOnlyOnce(t *testing.T) {
	target := buildinfo.Source{Revision: strings.Repeat("b", 40), State: "clean"}
	available := true
	n := updateFixture(&target, &available)
	results := make([]*mcp.CallToolResult, 16)
	var wg sync.WaitGroup
	for i := range results {
		results[i] = updateResult(map[string]string{"state": "pending"})
		wg.Add(1)
		go func() { defer wg.Done(); n.Apply(results[i]) }()
	}
	wg.Wait()
	count := 0
	for _, r := range results {
		if strings.Contains(r.Content[0].(*mcp.TextContent).Text, `"client_update"`) {
			count++
		}
	}
	if count != 1 {
		t.Fatal("concurrent responses duplicated notification", count)
	}
}
