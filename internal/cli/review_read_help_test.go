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

func TestReviewReadHelpOfflineWithMissingMalformedProfilesAndCancelledContext(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	malformed := filepath.Join(t.TempDir(), "malformed-profiles.json")
	original := []byte("PRIVATE_SYNTHETIC_INVALID_PROFILE")
	if err := os.WriteFile(malformed, original, 0600); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"queue", "assess", "source-assess", "ci-assess", "readiness"} {
		for _, spelling := range [][]string{{"reviews", operation, "--help"}, {"help", "reviews", operation}} {
			for _, profile := range []string{malformed, filepath.Join(t.TempDir(), "missing-profiles.json")} {
				args := append([]string{"--profile=bound-role", "--profiles-file", profile}, spelling...)
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				var stdout, stderr bytes.Buffer
				code := RunContext(ctx, args, map[string]string{"FC_SERVER": service.URL}, &stdout, &stderr)
				var got operationHelpResponse
				if code != ExitSuccess || stderr.Len() != 0 || stdout.Len() > 4096 || json.Unmarshal(stdout.Bytes(), &got) != nil || !strings.Contains(got.Usage, "reviews "+operation) || len(got.Semantics) == 0 || got.Example == "" || got.Documentation == "" {
					t.Fatalf("selected read help failed before profile access: operation=%s code=%d stdout=%s stderr=%s", operation, code, &stdout, &stderr)
				}
				for _, private := range []string{profile, service.URL, string(original)} {
					if strings.Contains(stdout.String(), private) {
						t.Fatal("help disclosed selected configuration")
					}
				}
			}
		}
	}
	if requests.Load() != 0 {
		t.Fatal("local help contacted a service")
	}
	if after, err := os.ReadFile(malformed); err != nil || !bytes.Equal(after, original) {
		t.Fatal("local help changed profile data", err)
	}
}

func TestReviewReadHelpMatchesActualArgumentForms(t *testing.T) {
	evidence := "review_evidence_0123456789abcdef0123456789abcdef"
	for _, args := range [][]string{{"reviews", "queue"}, {"reviews", "queue", "--limit", "20", "--cursor", "retained-page"}, {"reviews", "assess", evidence}, {"reviews", "source-assess", "--pr", "42"}, {"reviews", "ci-assess", "--pr=42"}, {"reviews", "readiness", "--pr", "42"}} {
		call, ok := parseInvocation(args)
		if !ok || call.command != "reviews" || call.helpOperation != "" || call.mutation != nil {
			t.Fatal("documented read does not select its existing parser", args)
		}
		switch args[1] {
		case "queue":
			if len(call.position) != 0 || call.snapshot != "" || len(args) == 2 && call.limit != 5 || len(args) > 2 && (call.limit != 20 || call.cursor != "retained-page") {
				t.Fatal("queue pagination syntax changed", args)
			}
		case "assess":
			if len(call.position) != 1 || call.position[0] != evidence {
				t.Fatal("captured assessment requires exact evidence identity")
			}
		default:
			if call.pullNumber != 42 {
				t.Fatal("stored assessment requires explicit --pr")
			}
		}
	}
	for _, args := range [][]string{{"reviews", "queue", "42"}, {"reviews", "queue", "--snapshot", "review_snapshot_0123456789abcdef0123456789abcdef"}, {"reviews", "assess", "--pr", "42"}, {"reviews", "source-assess", "42"}, {"reviews", "ci-assess", "42"}, {"reviews", "readiness", "42"}, {"reviews", "queue", "--help", "--limit", "20"}, {"reviews", "assess", evidence, "--help"}} {
		if _, ok := parseInvocation(args); ok {
			t.Fatal("help introduced an unsupported read/help argument form", args)
		}
	}
	for _, operation := range []string{"queue", "assess", "source-assess", "ci-assess", "readiness"} {
		help, ok := operationHelp("reviews " + operation)
		if !ok {
			t.Fatal("missing selected read help", operation)
		}
		example := strings.Fields(strings.TrimPrefix(help.Example, "<bound launcher> "))
		if call, valid := parseInvocation(example); !valid || call.command != "reviews" || call.subcommand != operation || call.mutation != nil {
			t.Fatal("help example is not the documented read", operation)
		}
	}
}

func TestReviewReadHelpSeparatesQueuePolicyAndReadiness(t *testing.T) {
	for operation, want := range map[string][]string{
		"queue":         {"default 5", "retained page size", "merged", "channel-front", "--cursor"},
		"assess":        {"immutable captured comparison", "current project policy", "CI", "threads"},
		"source-assess": {"latest retained source comparison", "current local review policy", "source-refresh --help"},
		"ci-assess":     {"required CI", "latest stored source comparison", "Review-channel policy"},
		"readiness":     {"finalized review coverage", "thread", "merge-state", "ready_as_observed", "source-refresh --help"},
	} {
		help, ok := operationHelp("reviews " + operation)
		data, _ := json.Marshal(help)
		if !ok {
			t.Fatal("missing purpose help", operation)
		}
		for _, term := range want {
			if !strings.Contains(string(data), term) {
				t.Fatalf("%s lacks purpose %q", operation, term)
			}
		}
		if !strings.Contains(strings.Join(help.Semantics, " "), "provider work") {
			t.Fatal("read help omits its local boundary", operation)
		}
	}
	queue, _ := operationHelp("reviews queue")
	if len(queue.Required) != 0 || strings.Contains(queue.Usage, "NUMBER") || strings.Contains(queue.Usage, "--snapshot") {
		t.Fatal("queue help advertises unsupported selectors")
	}
}

func TestReviewReadHelpDiscoveryRemainsLocalAndSeparatesExternalRefresh(t *testing.T) {
	for _, args := range [][]string{{"reviews", "help"}, {"reviews", "--help"}, {"help", "reviews"}} {
		args = append(args, "--profiles-file", filepath.Join(t.TempDir(), "missing.json"), "--profile", "bound")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var stdout, stderr bytes.Buffer
		if code := RunContext(ctx, args, nil, &stdout, &stderr); code != ExitSuccess || stderr.Len() != 0 || stdout.Len() > 4096 {
			t.Fatal("review discovery failed offline", code, &stderr)
		}
		for _, operation := range []string{"queue", "assess", "source-assess", "ci-assess", "readiness", "source-refresh"} {
			if !strings.Contains(stdout.String(), "reviews "+operation+" --help") {
				t.Fatal("review discovery omitted command help", operation)
			}
		}
		if !strings.Contains(stdout.String(), "source-refresh explicitly reads GitHub") {
			t.Fatal("review discovery conflates stored reads and explicit external refresh")
		}
	}
}

func TestReviewRoundReadHelpRemainsLocalWithExactSelectors(t *testing.T) {
	for _, spelling := range [][]string{{"reviews", "rounds", "--help"}, {"help", "reviews", "rounds"}, {"reviews", "rounds", "read", "--help"}, {"help", "reviews", "rounds", "read"}} {
		args := append([]string{"--profiles-file", "/missing/PRIVATE_ROUND_PROFILE", "--profile", "builder"}, spelling...)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var stdout, stderr bytes.Buffer
		code := RunContext(ctx, args, nil, &stdout, &stderr)
		var help operationHelpResponse
		if code != ExitSuccess || stderr.Len() != 0 || stdout.Len() > 4096 || json.Unmarshal(stdout.Bytes(), &help) != nil || help.Usage != "<bound launcher> reviews rounds read --pr NUMBER --channel cli|hosted" || help.Documentation != "docs/review-round-controls.md" || strings.Contains(stdout.String(), "PRIVATE") {
			t.Fatal("round-read discovery reached profile access or lost its selectors", spelling, code, &stdout, &stderr)
		}
		if call, ok := parseInvocation(strings.Fields(strings.TrimPrefix(help.Example, "<bound launcher> "))); !ok || call.rounds == nil || call.rounds.operation != "read" || call.rounds.pull != 42 {
			t.Fatal("help example does not select the existing read parser")
		}
	}
	group, ok := operationHelp("reviews help")
	if !ok || !strings.Contains(strings.Join(group.Optional, " "), "reviews rounds --help") {
		t.Fatal("review discovery omitted the current-control read")
	}
	for _, args := range [][]string{
		{"reviews", "rounds", "read", "--help", "--pr", "42"},
		{"reviews", "rounds", "read", "--pr", "--help", "--channel", "cli"},
		{"reviews", "rounds", "read", "--pr", "42", "--channel", "--help"},
		{"help", "reviews", "rounds", "read", "extra"},
		{"reviews", "rounds", "grant", "--help"},
	} {
		if _, ok := parseInvocation(args); ok {
			t.Fatal("help expanded unsupported argument grammar", args)
		}
	}
}
