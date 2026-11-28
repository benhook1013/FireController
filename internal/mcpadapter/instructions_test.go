package mcpadapter_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connectWithInstructions(t *testing.T, ctx context.Context, transport mcp.Transport, version string) *mcp.ClientSession {
	t.Helper()
	skill, err := os.ReadFile("../../skills/firecontroller-use/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, hasStart := strings.Cut(string(skill), "<!-- BEGIN MCP SERVER INSTRUCTIONS -->")
	core, _, hasEnd := strings.Cut(section, "<!-- END MCP SERVER INSTRUCTIONS -->")
	want := strings.TrimSpace(core)
	if !hasStart || !hasEnd || want == "" || len(want) > 1400 {
		t.Fatal("usage skill must provide short, delimited MCP instructions")
	}
	for _, term := range []string{"job_read", "job_update", "wait_for_ci", "firecontroller-reviews"} {
		if !strings.Contains(want, term) {
			t.Errorf("MCP instructions omit %q", term)
		}
	}

	var methods []string
	var wireInstructions string
	sdk := mcp.NewClient(&mcp.Implementation{Name: "guidance-fixture", Version: "1"}, nil)
	sdk.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			result, err := next(ctx, method, req)
			if method == "initialize" || method == "server/discover" {
				methods = append(methods, method)
			}
			switch result := result.(type) {
			case *mcp.InitializeResult:
				wireInstructions = result.Instructions
			case *mcp.DiscoverResult:
				wireInstructions = result.Instructions
			}
			return result, err
		}
	})
	session, err := sdk.Connect(ctx, transport, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	method := "initialize"
	if version == "2026-07-28" {
		method = "server/discover"
	}
	if len(methods) != 1 || methods[0] != method {
		t.Fatalf("want exactly %s, saw %v", method, methods)
	}
	if wireInstructions != want || session.InitializeResult().Instructions != want {
		t.Fatal("handshake instructions differ from the canonical skill core")
	}
	return session
}
