package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestReviewActivityCLISelectorsAndOfflineHelp(t *testing.T) {
	for _, args := range [][]string{nil, {"--hours", "1"}, {"--since", "2025-01-01T00:00:00Z", "--until", "2025-01-02T00:00:00Z", "--timezone", "Pacific/Auckland", "--pr", "7"}} {
		call, ok := parseInvocation(append([]string{"reviews", "activity"}, args...))
		if !ok || call.mutation != nil {
			t.Fatal("read selector parsing", call, ok)
		}
	}
	for _, args := range [][]string{{"--hours", "0"}, {"--hours", "169"}, {"--hours", "1", "--hours", "2"}, {"--since", "2025-01-01T00:00:00Z"}, {"--timezone", "Local"}, {"--timezone", ""}, {"--hours", "1", "--since", "2025-01-01T00:00:00Z", "--until", "2025-01-02T00:00:00Z"}, {"--pr", "0"}, {"--key", "forbidden"}} {
		var out, diag bytes.Buffer
		if code := RunContext(t.Context(), append([]string{"--profiles-file", "/missing", "reviews", "activity"}, args...), nil, &out, &diag); code != ExitUsage || out.Len() != 0 || !strings.Contains(diag.String(), "invalid_arguments") {
			t.Fatal("invalid activity reached profiles", args, code, out.String(), diag.String())
		}
	}
	var out, diag bytes.Buffer
	if code := RunContext(t.Context(), []string{"--profiles-file", "/missing", "reviews", "activity", "--help"}, nil, &out, &diag); code != 0 || diag.Len() != 0 || !strings.Contains(out.String(), "docs/review-activity.md") {
		t.Fatal(code, out.String(), diag.String())
	}
}
