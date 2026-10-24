package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestInboxListHelpIsOfflineAndExplainsPendingDiscovery(t *testing.T) {
	for _, args := range [][]string{{"inbox", "list", "--help"}, {"help", "inbox", "list"}} {
		args = append([]string{"--profile", "bound", "--profiles-file", filepath.Join(t.TempDir(), "missing.json")}, args...)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var stdout, stderr bytes.Buffer
		code := RunContext(ctx, args, nil, &stdout, &stderr)
		var help operationHelpResponse
		if code != ExitSuccess || stderr.Len() != 0 || stdout.Len() > 2048 || json.Unmarshal(stdout.Bytes(), &help) != nil || len(help.Required) != 0 || len(help.Optional) != 5 {
			t.Fatalf("offline list help: code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
		}
		for _, text := range []string{"oldest first", "insertion order", "no recent-first", "seen-but-unhandled", "--unread", "same filters", "inbox snapshot", "event_cursor", "acknowledgment"} {
			if !strings.Contains(stdout.String(), text) {
				t.Fatalf("list help omits %q", text)
			}
		}
		example := strings.Fields(strings.TrimPrefix(help.Example, "<bound launcher> "))
		call, ok := parseInvocation(example)
		if !ok || call.command != "inbox" || call.subcommand != "list" || !call.unread || !call.unack || call.limit != 20 {
			t.Fatalf("discovery example does not select unread pending messages: %+v", call)
		}
	}
}

func TestInboxListHelpRejectsUnsupportedArgumentsWithoutReadingProfiles(t *testing.T) {
	for _, args := range [][]string{
		{"inbox", "list", "--help", "extra"},
		{"inbox", "list", "--unread", "--help"},
		{"inbox", "list", "--all", "--unread"},
		{"inbox", "list", "--sort", "newest"},
		{"inbox", "list", "--private-typo=DO_NOT_ECHO"},
	} {
		var stdout, stderr bytes.Buffer
		code := RunContext(t.Context(), args, nil, &stdout, &stderr)
		if code != ExitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), "inbox list --help") || strings.Contains(stderr.String(), "DO_NOT_ECHO") {
			t.Fatalf("unsupported list command: %v code=%d stdout=%s stderr=%s", args, code, &stdout, &stderr)
		}
	}
}
