package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestActionsRunDiagnosticCLISelectorsAndOfflineHelp(t *testing.T) {
	for _, args := range [][]string{{"--run", "50", "--attempt", "2"}, {"--run=50", "--attempt=2", "--limit=20"}} {
		call, ok := parseInvocation(append([]string{"reviews", "actions-run"}, args...))
		if !ok || call.actionsRunDiagnostic == nil || call.mutation != nil {
			t.Fatal(call, ok)
		}
	}
	for _, args := range [][]string{nil, {"--run", "50"}, {"--run", "0", "--attempt", "2"}, {"--run", "01", "--attempt", "2"}, {"--run", "-1", "--attempt", "2"}, {"--run", "50", "--attempt", "0"}, {"--run", "50", "--attempt", "2", "--limit", "0"}, {"--run", "50", "--attempt", "2", "--limit", "21"}, {"--run", "50", "--attempt", "2", "--run", "51"}, {"--run", "50", "--attempt", "2", "--repository", "other/repo"}, {"--run", "50", "--attempt", "2", "--cancel"}, {"--run", "50", "--attempt", "2", "--key", "no"}} {
		var out, diag bytes.Buffer
		code := RunContext(t.Context(), append([]string{"--profiles-file", "/missing", "reviews", "actions-run"}, args...), nil, &out, &diag)
		if code != ExitUsage || out.Len() != 0 || !strings.Contains(diag.String(), "invalid_arguments") {
			t.Fatal("invalid args reached profile", args, code, out.String(), diag.String())
		}
	}
	var out, diag bytes.Buffer
	if code := RunContext(t.Context(), []string{"--profiles-file", "/missing", "reviews", "actions-run", "--help"}, nil, &out, &diag); code != 0 || diag.Len() != 0 || !strings.Contains(out.String(), "No logs") || !strings.Contains(out.String(), "docs/provider-observations.md") {
		t.Fatal(code, out.String(), diag.String())
	}
}
