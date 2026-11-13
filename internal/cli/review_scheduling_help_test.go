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

func TestReviewSchedulingLeafHelpIsOffline(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	path := filepath.Join(t.TempDir(), "profiles.json")
	private := []byte("PRIVATE_INVALID_PROFILE")
	if err := os.WriteFile(path, private, 0600); err != nil {
		t.Fatal(err)
	}
	for _, leaf := range []string{"read", "configure", "pause", "resume"} {
		for _, spelling := range [][]string{{"reviews", "scheduling", leaf, "--help"}, {"help", "reviews", "scheduling", leaf}} {
			for _, profilePath := range []string{path, filepath.Join(t.TempDir(), "absent.json")} {
				args := append([]string{"--profiles-file", profilePath, "--profile", "bound"}, spelling...)
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				var stdout, stderr bytes.Buffer
				code := RunContext(ctx, args, map[string]string{"FC_SERVER": service.URL}, &stdout, &stderr)
				var help operationHelpResponse
				if code != ExitSuccess || stderr.Len() != 0 || stdout.Len() > 2500 || json.Unmarshal(stdout.Bytes(), &help) != nil || !strings.HasPrefix(help.Usage, "<bound launcher> reviews scheduling "+leaf) || help.Documentation != "docs/review-scheduling.md" {
					t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, &stdout, &stderr)
				}
				if leaf != "read" && (!strings.Contains(help.Usage, "--expect-revision N --key KEY") || !strings.Contains(strings.Join(help.Semantics, " "), "same key, exact request")) {
					t.Fatal("mutation help omitted revision/key recovery", leaf, help)
				}
				if leaf == "pause" && !strings.Contains(help.Usage, "[--reason TEXT]") || leaf == "resume" && strings.Contains(help.Usage, "--reason") {
					t.Fatal("pause/resume reason syntax drifted", help)
				}
				if call, ok := parseInvocation(strings.Fields(strings.TrimPrefix(help.Example, "<bound launcher> "))); !ok || call.command == "help" || call.reviewScheduling == nil || call.reviewScheduling.action != leaf {
					t.Fatal("help example does not select its real scheduling operation", leaf, call, ok)
				}
			}
		}
	}
	if requests.Load() != 0 {
		t.Fatal("help contacted a server")
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, private) {
		t.Fatal("help changed selected configuration", err)
	}
	parent, _ := operationHelp("reviews scheduling")
	for _, leaf := range []string{"read", "configure", "pause", "resume"} {
		if !strings.Contains(strings.Join(parent.Optional, " "), "reviews scheduling "+leaf+" --help") {
			t.Fatal("parent help does not discover leaf", leaf)
		}
	}
}

func TestReviewSchedulingLeafHelpKeepsExactGrammar(t *testing.T) {
	for _, args := range [][]string{
		{"reviews", "scheduling", "resume", "--help", "extra"},
		{"reviews", "scheduling", "pause", "--help", "--pr", "42"},
		{"reviews", "scheduling", "configure", "--help=true"},
		{"reviews", "scheduling", "read", "--pr", "42", "--help"},
		{"help", "reviews", "scheduling", "unknown"},
		{"help", "reviews", "scheduling", "resume", "--key", "help-is-not-a-write"},
		{"reviews", "scheduling", "resume", "--help", "--profile=a", "--profile=b"},
	} {
		var stdout, stderr bytes.Buffer
		if code := RunContext(t.Context(), args, nil, &stdout, &stderr); code != ExitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), "invalid_arguments") {
			t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, &stdout, &stderr)
		}
	}
	call, ok := parseInvocation([]string{"reviews", "scheduling", "pause", "--pr", "42", "--expect-revision", "0", "--key", "pause-1", "--reason", "--help"})
	if !ok || call.command == "help" || call.reviewScheduling == nil || call.reviewScheduling.pause.Reason != "--help" {
		t.Fatal("help-looking reason was treated as local help", call, ok)
	}
}
