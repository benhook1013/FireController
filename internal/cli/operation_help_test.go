package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestSelectionHelpNeedsNoProfileOrConnection(t *testing.T) {
	for _, operation := range []string{"start", "run-read"} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var out, errout bytes.Buffer
		code := RunContext(ctx, []string{"--profiles-file", "/missing/private-profile.json", "reviews", operation, "--help"}, nil, &out, &errout)
		if code != ExitSuccess || errout.Len() != 0 || !strings.Contains(out.String(), "reviews "+operation) || out.Len() > 4096 || strings.Contains(out.String(), "private-profile") {
			t.Fatalf("%s: %d %s %s", operation, code, &out, &errout)
		}
	}
}
