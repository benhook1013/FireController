package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestLaneDiagnosticCLIReadSyntax(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		call, ok := parseInvocation([]string{"reviews", "scheduling", "diagnostics", "--channel", channel})
		if !ok || call.reviewScheduling == nil || call.reviewScheduling.action != "diagnostics" || call.reviewScheduling.channel != channel {
			t.Fatal(call, ok)
		}
	}
	for _, args := range [][]string{{}, {"--channel", "manual"}, {"--channel", "cli", "--key", "no"}, {"--channel", "cli", "--channel", "hosted"}, {"--channel", "cli", "--pr", "2"}} {
		if _, ok := parseReviewScheduling(append([]string{"diagnostics"}, args...)); ok {
			t.Fatal("invalid read accepted", args)
		}
	}
	var out, diag bytes.Buffer
	if code := RunContext(t.Context(), []string{"--profiles-file", "/missing", "reviews", "scheduling", "diagnostics", "--help"}, nil, &out, &diag); code != 0 || diag.Len() != 0 || !strings.Contains(out.String(), "not fresh admission") {
		t.Fatal(code, out.String(), diag.String())
	}
}
