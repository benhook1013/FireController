package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestActionsDiagnosticCLIReadSelectorsAndOfflineHelp(t *testing.T) {
	for _, args := range [][]string{nil, {"--age-hours", "1", "--limit", "1"}, {"--age-hours=720", "--limit=20"}} {
		call, ok := parseInvocation(append([]string{"reviews", "actions-liveness"}, args...))
		if !ok || call.mutation != nil || call.actionsDiagnostic == nil {
			t.Fatal("diagnostic was not parsed as explicit read", call, ok)
		}
	}
	for _, args := range [][]string{{"--age-hours", "0"}, {"--age-hours", "721"}, {"--age-hours", "-1"}, {"--age-hours", "01"}, {"--age-hours", "+1"}, {"--age-hours"}, {"--age-hours", "1", "--age-hours=2"}, {"--limit", "0"}, {"--limit", "21"}, {"--limit=1", "--limit", "2"}, {"--key", "forbidden"}, {"--repository", "other/repo"}, {"--cancel"}, {"--watch"}, {"extra"}} {
		var out, diag bytes.Buffer
		code := RunContext(t.Context(), append([]string{"--profiles-file", "/missing", "reviews", "actions-liveness"}, args...), nil, &out, &diag)
		if code != ExitUsage || out.Len() != 0 || !strings.Contains(diag.String(), "invalid_arguments") {
			t.Fatal("invalid diagnostic reached profile setup", args, code, out.String(), diag.String())
		}
	}
	var out, diag bytes.Buffer
	code := RunContext(t.Context(), []string{"--profiles-file", "/missing", "reviews", "actions-liveness", "--help"}, nil, &out, &diag)
	if code != 0 || diag.Len() != 0 || !strings.Contains(out.String(), "docs/provider-observations.md") || !strings.Contains(out.String(), "capacity is unknown") || !strings.Contains(out.String(), "No cancellation") {
		t.Fatal("offline help lost diagnostic limits", code, out.String(), diag.String())
	}
}
