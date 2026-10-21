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

	"github.com/benhook1013/FireController/internal/client"
)

func TestOperationHelpIsOfflineBoundedAndDoesNotReadFiles(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	path := filepath.Join(t.TempDir(), "profiles.json")
	before := []byte("PRIVATE_INVALID_PROFILE")
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"jobs create", "jobs update", "jobs checklist-item", "inbox send", "notes add", "notes update", "map update", "publication prepare"} {
		for _, spelling := range [][]string{append(strings.Fields(operation), "--help"), append([]string{"help"}, strings.Fields(operation)...)} {
			for _, profilePath := range []string{path, filepath.Join(t.TempDir(), "missing.json")} {
				args := append([]string{"--profile=bound-role", "--profiles-file", profilePath}, spelling...)
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				var stdout, stderr bytes.Buffer
				code := RunContext(ctx, args, map[string]string{"FC_SERVER": service.URL}, &stdout, &stderr)
				var result operationHelpResponse
				if code != ExitSuccess || stderr.Len() != 0 || stdout.Len() > 4096 || json.Unmarshal(stdout.Bytes(), &result) != nil || !strings.Contains(result.Usage, operation) || len(result.Required) == 0 || len(result.Semantics) == 0 || result.Example == "" {
					t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, &stdout, &stderr)
				}
				if strings.Contains(stdout.String(), profilePath) || strings.Contains(stdout.String(), service.URL) || strings.Contains(stdout.String(), string(before)) {
					t.Fatal("help leaked selected configuration")
				}
			}
		}
	}
	if requests.Load() != 0 {
		t.Fatal("help contacted a service")
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
		t.Fatal("help changed configuration", err)
	}
}

func TestOperationHelpRequiresExactGrammarAndValidGlobalFlags(t *testing.T) {
	for _, args := range [][]string{
		{"jobs", "update", "--help", "extra"}, {"jobs", "update", "--help=true"},
		{"jobs", "update", "--help", "--profile=a", "--profile=b"},
		{"jobs", "update", "--help", "--profile"},
		{"jobs", "update", "--help", "--profiles-file="},
		{"jobs", "update", "--help", "--profile= padded "},
		{"jobs", "update", "--help", "--profiles-file=bad\npath"},
		{"help", "jobs", "update", "--unknown"}, {"help", "unknown", "operation"},
		{"notes", "update", "NOTE_ID", "--help"},
		{"publication", "prepare", "--file", "/private/not-read", "--help"},
	} {
		var stdout, stderr bytes.Buffer
		if code := RunContext(t.Context(), args, nil, &stdout, &stderr); code != ExitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), "invalid_arguments") {
			t.Fatalf("%v: %d %s %s", args, code, &stdout, &stderr)
		}
	}
}

func TestOperationErrorsPointToSelectedHelpWithoutEchoingArguments(t *testing.T) {
	for _, operation := range []string{"jobs create", "jobs update", "jobs checklist-item", "inbox send", "notes add", "notes update", "map update", "publication prepare"} {
		var stdout, stderr bytes.Buffer
		args := append(strings.Fields(operation), "--private-typo=DO_NOT_ECHO")
		if code := RunContext(t.Context(), args, map[string]string{"FC_PROFILE": "missing"}, &stdout, &stderr); code != ExitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), operation+" --help") || strings.Contains(stderr.String(), "DO_NOT_ECHO") {
			t.Fatalf("%v: %d %s %s", args, code, &stdout, &stderr)
		}
	}
}

func TestHelpLookingFieldValuesAreStillData(t *testing.T) {
	id := "job_0123456789abcdef0123456789abcdef"
	note := "note_0123456789abcdef0123456789abcdef"
	for _, args := range [][]string{
		{"jobs", "update", id, "--expect-revision", "1", "--key", "test", "--brief", "--help"},
		{"jobs", "update", id, "--expect-revision=1", "--key=test", "--next=--help"},
		{"notes", "add", "--key", "test", "--body", "--help"},
		{"notes", "update", note, "--expect-revision", "1", "--key", "test", "--body", "--help"},
	} {
		call, ok := parseInvocation(args)
		if !ok || call.command == "help" || call.helpOperation != "" {
			t.Fatalf("ordinary help-looking data changed interpretation: %v", args)
		}
		var stdout, stderr bytes.Buffer
		// A normal command reaches profile selection rather than local help.
		if code := RunContext(context.Background(), args, nil, &stdout, &stderr); code != ExitConfig || stdout.Len() != 0 || !strings.Contains(stderr.String(), "profile_required") {
			t.Fatalf("%v: %d %s %s", args, code, &stdout, &stderr)
		}
	}
}

func TestOperationHelpExamplesMatchRealParsers(t *testing.T) {
	id := "job_0123456789abcdef0123456789abcdef"
	note := "note_0123456789abcdef0123456789abcdef"
	mapID := "map_0123456789abcdef0123456789abcdef"
	for _, args := range [][]string{
		{"jobs", "update", id, "--expect-revision", "3", "--key", "job-next-1", "--next", "Run focused validation"},
		{"notes", "add", "--body", "Revisit after the API settles", "--key", "reminder-1"},
		{"notes", "update", note, "--expect-revision", "2", "--key", "reminder-consumed-1", "--status", "consumed"},
		{"map", "update", mapID, "--expect-revision", "3", "--file", filepath.Join(t.TempDir(), "changes.json"), "--key", "project-state-1"},
		{"publication", "prepare", "--file", filepath.Join(t.TempDir(), "changes.json"), "--output", filepath.Join(t.TempDir(), "approved.json")},
	} {
		if _, ok := parseInvocation(args); !ok {
			t.Fatalf("documented example refused: %v", args)
		}
	}
	mapHelp, _ := operationHelp("map update")
	mapRaw, _ := json.Marshal(mapHelp.FileExample)
	if _, err := client.DecodeProjectMapChanges(mapRaw); err != nil {
		t.Fatal("documented map file refused", err)
	}
	pubHelp, _ := operationHelp("publication prepare")
	pubRaw, _ := json.Marshal(pubHelp.FileExample)
	if _, err := client.DecodePublicationChanges(pubRaw); err != nil {
		t.Fatal("documented publication plan refused", err)
	}
}

func TestOperationHelpAdvertisedEditsReachExistingSemantics(t *testing.T) {
	job := "job_0123456789abcdef0123456789abcdef"
	worker := "worker_0123456789abcdef0123456789abcdef"
	note := "note_0123456789abcdef0123456789abcdef"
	for _, edit := range [][]string{
		{"--title", "A useful title"}, {"--brief", ""}, {"--worker", worker}, {"--worker", ""},
		{"--status", "active"}, {"--status", "blocked"}, {"--status", "parked"}, {"--status", "completed"},
		{"--progress", ""}, {"--next", ""}, {"--blocker", ""}, {"--evidence", ""},
		{"--evidence", "first", "--evidence", "second"},
	} {
		args := append([]string{"jobs", "update", job, "--expect-revision", "2", "--key", "edit"}, edit...)
		if call, ok := parseInvocation(args); !ok || call.mutation == nil || call.command == "help" {
			t.Fatalf("advertised job edit refused: %v", args)
		}
	}
	bodyPath := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(bodyPath, []byte("Retained reminder"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, edit := range [][]string{
		{"--worker", worker}, {"--clear-worker"}, {"--job", job}, {"--clear-job"},
		{"--phase", "later"}, {"--clear-phase"}, {"--kind", "reminder"}, {"--kind", "source"},
		{"--body", "Updated reminder"}, {"--body-file", bodyPath},
		{"--status", "pending"}, {"--status", "consumed"}, {"--status", "dismissed", "--reason", "No longer relevant"},
		{"--reason", ""},
	} {
		args := append([]string{"notes", "update", note, "--expect-revision", "2", "--key", "edit"}, edit...)
		if call, ok := parseInvocation(args); !ok || call.notes == nil || call.command == "help" {
			t.Fatalf("advertised note edit refused: %v", args)
		}
	}
	// Body-file choice and explicit non-worker scopes are also valid on add.
	for _, args := range [][]string{
		{"notes", "add", "--key", "add", "--body-file", bodyPath},
		{"notes", "add", "--key", "add", "--body", "Reminder", "--clear-worker", "--job", job},
		{"notes", "add", "--key", "add", "--body", "Reminder", "--clear-worker", "--job", job, "--phase", "later", "--kind", "source"},
	} {
		if _, ok := parseInvocation(args); !ok {
			t.Fatalf("advertised note creation refused: %v", args)
		}
	}
}
