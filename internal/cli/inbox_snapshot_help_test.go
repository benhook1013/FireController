package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestInboxSnapshotHelpOfflineWithoutCredentials(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	root := t.TempDir()
	invalidPath := filepath.Join(root, "invalid.json")
	private := []byte("PRIVATE_INVALID_PROFILE")
	if err := os.WriteFile(invalidPath, private, 0600); err != nil {
		t.Fatal(err)
	}
	absentCredential := filepath.Join(root, "missing.token")
	configuredPath := filepath.Join(root, "profiles.json")
	configuration := map[string]any{"version": 1, "profiles": map[string]any{"bound": map[string]string{"server_url": service.URL, "instance_id": "instance-one", "project_id": "project-one", "worker_id": "worker-one", "credential_file": absentCredential}}}
	configured, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configuredPath, configured, 0600); err != nil {
		t.Fatal(err)
	}
	for _, profilePath := range []string{invalidPath, configuredPath, filepath.Join(root, "missing.json")} {
		for _, spelling := range [][]string{{"inbox", "snapshot", "--help"}, {"help", "inbox", "snapshot"}} {
			args := append([]string{"--profile", "bound", "--profiles-file", profilePath}, spelling...)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			var stdout, stderr bytes.Buffer
			code := RunContext(ctx, args, nil, &stdout, &stderr)
			var help operationHelpResponse
			if code != ExitSuccess || stderr.Len() != 0 || stdout.Len() > 2048 || json.Unmarshal(stdout.Bytes(), &help) != nil || len(help.Required) != 0 || len(help.Optional) != 6 || help.Documentation != "docs/message-events.md" {
				t.Fatalf("offline help failed: %v code%d out%s err%s", args, code, &stdout, &stderr)
			}
			for _, text := range []string{"pending, unacknowledged", "seen-but-unhandled", "event_cursor", "inbox.cursor", "same filters", "acknowledgment"} {
				if !strings.Contains(stdout.String(), text) {
					t.Fatalf("help omits %q", text)
				}
			}
			if strings.Contains(stdout.String(), string(private)) || strings.Contains(stdout.String(), service.URL) || strings.Contains(stdout.String(), profilePath) || strings.Contains(stdout.String(), absentCredential) {
				t.Fatal("help leaked configuration")
			}
		}
	}
	if requests.Load() != 0 {
		t.Fatal("local help contacted service")
	}
	if after, err := os.ReadFile(invalidPath); err != nil || !bytes.Equal(after, private) {
		t.Fatal("help changed invalid profile", err)
	}
	if after, err := os.ReadFile(configuredPath); err != nil || !bytes.Equal(after, configured) {
		t.Fatal("help changed configured profile", err)
	}
	if _, err := os.Stat(absentCredential); !os.IsNotExist(err) {
		t.Fatal("help created or accessed replacement credentials", err)
	}
}

func TestInboxSnapshotHelpAndMalformedReadGrammar(t *testing.T) {
	for _, args := range [][]string{
		{"inbox", "snapshot", "--help", "extra"}, {"inbox", "snapshot", "--help=true"}, {"inbox", "snapshot", "--unread", "--help"}, {"help", "inbox", "snapshot", "--all"},
		{"inbox", "snapshot", "--help", "--profile=a", "--profile=b"}, {"inbox", "snapshot", "--help", "--profiles-file="},
		{"inbox", "snapshot", "--limit", "0"}, {"inbox", "snapshot", "--limit", "101"}, {"inbox", "snapshot", "--limit", "1", "--limit", "2"},
		{"inbox", "snapshot", "--all", "--unread"}, {"inbox", "snapshot", "--all", "--unacknowledged"}, {"inbox", "snapshot", "--unacknowledged=true"}, {"inbox", "snapshot", "--cursor"},
	} {
		var stdout, stderr bytes.Buffer
		if code := RunContext(t.Context(), args, nil, &stdout, &stderr); code != ExitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), "invalid_arguments") {
			t.Fatalf("malformed command admitted: %v code%d out%s err%s", args, code, &stdout, &stderr)
		}
	}
}

func TestInboxSnapshotHelpExamplesMatchParserAndPendingDefaults(t *testing.T) {
	help, ok := operationHelp("inbox snapshot")
	if !ok {
		t.Fatal("missing operation help")
	}
	example := strings.Fields(strings.TrimPrefix(help.Example, "<bound launcher> "))
	for _, test := range []struct {
		args          []string
		unread, unack bool
		limit         int
		cursor        string
	}{
		{example, false, true, 20, ""},
		{[]string{"inbox", "snapshot"}, false, true, 0, ""},
		{[]string{"inbox", "snapshot", "--unread"}, true, true, 0, ""},
		{[]string{"inbox", "snapshot", "--all"}, false, false, 0, ""},
		{[]string{"inbox", "snapshot", "--unread", "--unacknowledged", "--limit=100", "--cursor=opaque-page-cursor"}, true, true, 100, "opaque-page-cursor"},
	} {
		call, valid := parseInvocation(test.args)
		if !valid || call.command != "inbox" || call.subcommand != "snapshot" || call.helpOperation != "" || call.unread != test.unread || call.unack != test.unack || call.limit != test.limit || call.cursor != test.cursor || len(call.position) != 0 {
			t.Fatalf("help example/default drift: %v %+v", test.args, call)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := RunContext(t.Context(), []string{"inbox", "snapshot", "--private-typo=DO_NOT_ECHO"}, nil, &stdout, &stderr); code != ExitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), "inbox snapshot --help") || strings.Contains(stderr.String(), "DO_NOT_ECHO") {
		t.Fatal("invalid snapshot args did not point to local help", code, &stderr)
	}
}
