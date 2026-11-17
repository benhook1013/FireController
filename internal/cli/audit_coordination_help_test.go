package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
)

func TestAuditWorkerAndFocusHelpIsOfflineAndActionable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "PRIVATE_MALFORMED_PROFILE")
	const private = "PRIVATE_NOT_CONFIGURATION"
	if err := os.WriteFile(path, []byte(private), 0600); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct {
		group, operation string
		required         []string
	}{
		{"workers", "list", []string{"bound project", "--limit", "--cursor", "has_more"}},
		{"workers", "pause-state", []string{"bound worker", "revision 0", "paused", "reason"}},
		{"workers", "pause", []string{"--expect-revision", "--key", "--reason", "1024", "review execution", "workers resume"}},
		{"workers", "resume", []string{"--expect-revision", "--key", "clears both paused and reason"}},
		{"workers", "pause-history", []string{"bound worker", "newest-revision-first", "has_more", "--cursor"}},
		{"jobs", "primary", []string{"bound worker", "resume focus", "revision 0", "completed", "jobs assigned"}},
		{"jobs", "primary-set", []string{"JOB_ID", "assigned to the bound worker", "focus revision", "not use the job revision", "--key"}},
		{"jobs", "primary-clear", []string{"focus revision", "--expect-revision", "--key"}},
		{"jobs", "primary-history", []string{"bound worker", "newest-revision-first", "has_more", "--cursor"}},
	} {
		t.Run(spec.group+"-"+spec.operation, func(t *testing.T) {
			for _, args := range [][]string{{spec.group, spec.operation, "--help"}, {"help", spec.group, spec.operation}} {
				args = append([]string{"--profiles-file", path, "--profile", "bound"}, args...)
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				var out, stderr bytes.Buffer
				code := cli.RunContext(ctx, args, nil, &out, &stderr)
				var help struct{ Usage, Example, Documentation string }
				if code != cli.ExitSuccess || stderr.Len() != 0 || out.Len() > 2048 || json.Unmarshal(out.Bytes(), &help) != nil || help.Usage == "" || help.Documentation == "" {
					t.Fatalf("offline help: %d %s %s", code, &out, &stderr)
				}
				for _, want := range spec.required {
					if !strings.Contains(out.String(), want) {
						t.Fatalf("help omits %q: %s", want, &out)
					}
				}
				if strings.Contains(out.String(), path) || strings.Contains(out.String(), private) {
					t.Fatal("help disclosed configuration")
				}
				// The advertised existing operation must parse before the deliberately malformed profile.
				example := strings.Fields(strings.TrimPrefix(help.Example, "<bound launcher> "))
				// The one reason-bearing example intentionally uses quoted text.
				if spec.operation == "pause" {
					example = []string{"workers", "pause", "--expect-revision", "0", "--key", "pause-1", "--reason", "Awaiting human direction"}
				}
				code, _, diagnostic := run(append([]string{"--profiles-file", path, "--profile", "bound"}, example...), nil)
				if code != cli.ExitConfig || !strings.Contains(diagnostic, "profiles_") {
					t.Fatalf("help example failed syntax: %v %d %s", example, code, diagnostic)
				}
			}
		})
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != private {
		t.Fatal("help changed profile")
	}
}

func TestAuditCoordinationDiscoveryAndErrorsPointToLocalHelp(t *testing.T) {
	for _, args := range [][]string{{"workers", "help"}, {"workers", "--help"}, {"help", "workers"}, {"jobs", "--help"}, {"--help"}} {
		code, out, stderr := run(args, nil)
		if code != cli.ExitSuccess || stderr != "" || len(out) > 4096 {
			t.Fatalf("discovery: %v %d %s %s", args, code, out, stderr)
		}
		if args[0] == "--help" {
			for _, want := range []string{"workers help", "jobs primary"} {
				if !strings.Contains(out, want) {
					t.Fatalf("default help omits %q", want)
				}
			}
		}
		if args[0] == "jobs" && !strings.Contains(out, "jobs primary-set --help") {
			t.Fatal("jobs group hides focus edits")
		}
	}
	for _, args := range [][]string{
		{"workers", "list", "--unknown=PRIVATE_INPUT"}, {"workers", "pause-state", "extra"}, {"workers", "pause", "--expect-revision", "0", "--key", "key"}, {"workers", "pause-history", "--limit", "101"},
		{"jobs", "primary", "--help", "extra"}, {"jobs", "primary-set", "job-1"}, {"jobs", "primary-history", "--unknown=PRIVATE_INPUT"},
	} {
		code, out, stderr := run(args, nil)
		if code != cli.ExitUsage || out != "" || strings.Contains(stderr, "PRIVATE_INPUT") || !strings.Contains(stderr, args[0]+" "+args[1]+" --help") {
			t.Fatalf("argument discovery: %v %d %s %s", args, code, out, stderr)
		}
	}
	// Help-looking reason text remains ordinary data rather than invoking help.
	code, _, stderr := run([]string{"workers", "pause", "--expect-revision", "0", "--key", "key", "--reason", "--help"}, nil)
	if code != cli.ExitConfig || !strings.Contains(stderr, "profile_required") {
		t.Fatalf("reason parsed as help: %d %s", code, stderr)
	}
}
