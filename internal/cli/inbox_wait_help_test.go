package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInboxWaitHelpOfflineWithoutConnection(t *testing.T) {
	root := t.TempDir()
	invalid := filepath.Join(root, "invalid-profiles.json")
	private := []byte("PRIVATE_INVALID_PROFILE")
	if err := os.WriteFile(invalid, private, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{invalid, filepath.Join(root, "missing-profiles.json")} {
		for _, spelling := range [][]string{{"inbox", "wait", "--help"}, {"help", "inbox", "wait"}} {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			args := append([]string{"--profile", "absent", "--profiles-file", path}, spelling...)
			var stdout, stderr bytes.Buffer
			code := RunContext(ctx, args, nil, &stdout, &stderr)
			var help operationHelpResponse
			if code != ExitSuccess || stderr.Len() != 0 || stdout.Len() > 2048 || json.Unmarshal(stdout.Bytes(), &help) != nil || len(help.Required) != 1 || len(help.Optional) != 2 || help.Documentation != "docs/message-wait.md" {
				t.Fatal("offline wait help failed", args, code, stdout.String(), stderr.String())
			}
			for _, text := range []string{"--cursor", "event_cursor", "inbox.cursor", "default 20", "default 25", "1-60", "timed_out", "saved cursor", "acknowledge"} {
				if !strings.Contains(stdout.String(), text) {
					t.Fatal("help omitted wait contract", text)
				}
			}
			if strings.Contains(stdout.String(), path) || strings.Contains(stdout.String(), string(private)) {
				t.Fatal("help exposed private configuration")
			}
		}
	}
	if after, err := os.ReadFile(invalid); err != nil || !bytes.Equal(after, private) {
		t.Fatal("help changed profile fixture", err)
	}
}

func TestInboxWaitHelpExactGrammarAndExistingDefaults(t *testing.T) {
	for _, args := range [][]string{{"inbox", "wait", "--help", "extra"}, {"inbox", "wait", "--help=true"}, {"inbox", "wait", "--cursor", "event-cursor", "--help"}, {"help", "inbox", "wait", "--limit", "5"}, {"inbox", "wait", "--help", "--profile=a", "--profile=b"}, {"inbox", "wait", "--help", "--profiles-file="}} {
		var stdout, stderr bytes.Buffer
		if code := RunContext(t.Context(), args, nil, &stdout, &stderr); code != ExitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), "invalid_arguments") {
			t.Fatal("invalid help grammar accepted", args, code, stdout.String(), stderr.String())
		}
	}
	help, ok := operationHelp("inbox wait")
	if !ok {
		t.Fatal("missing wait help")
	}
	example := strings.Fields(strings.TrimPrefix(help.Example, "<bound launcher> "))
	for _, test := range []struct {
		args           []string
		limit, seconds int
	}{
		{example, 20, 25},
		{[]string{"inbox", "wait", "--cursor", "event-cursor"}, 20, 25},
		{[]string{"inbox", "wait", "--cursor", "event-cursor", "--limit", "100", "--wait-seconds", "60"}, 100, 60},
	} {
		call, valid := parseInvocation(test.args)
		if !valid || call.command != "inbox" || call.subcommand != "wait" || call.limit != test.limit || call.waitSeconds != test.seconds || call.helpOperation != "" {
			t.Fatal("help changed wait grammar/defaults", test.args, call)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := RunContext(t.Context(), []string{"inbox", "wait", "--private-typo=DO_NOT_ECHO"}, nil, &stdout, &stderr); code != ExitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), "inbox wait --help") || strings.Contains(stderr.String(), "DO_NOT_ECHO") {
		t.Fatal("invalid wait args missed local help", code, stdout.String(), stderr.String())
	}
}
