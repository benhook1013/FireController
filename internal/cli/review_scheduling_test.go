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

func TestReviewSchedulingCLIExplicitControlsBeforeProfile(t *testing.T) {
	for _, args := range [][]string{
		nil, {"--pr", "7"}, {"read", "--pr=7"},
		{"configure", "--automatic-hosted", "false", "--expect-revision", "0", "--key", "disable"},
		{"configure", "--automatic-hosted=true", "--expect-revision=3", "--key=enable"},
		{"pause", "--pr", "7", "--expect-revision", "0", "--reason", "Preparing changes", "--key", "pause"},
		{"resume", "--pr", "7", "--expect-revision", "1", "--key", "resume"},
	} {
		call, ok := parseInvocation(append([]string{"reviews", "scheduling"}, args...))
		if !ok || call.reviewScheduling == nil || call.command != "reviews" {
			t.Fatal("supported scheduling syntax refused", args, call, ok)
		}
	}
	for _, args := range [][]string{
		{"configure", "--automatic-hosted", "true", "--key", "missing-revision"},
		{"configure", "--expect-revision", "0", "--key", "missing-explicit-bool"},
		{"configure", "--automatic-hosted", "yes", "--expect-revision", "0", "--key", "bad-bool"},
		{"configure", "--automatic-hosted", "true", "--expect-revision", "-1", "--key", "bad-revision"},
		{"--pr", "0"}, {"--pr", "07"}, {"--pr", "7", "--pr", "8"}, {"--key", "read-mutation"},
		{"pause", "--pr", "7", "--expect-revision", "0"},
		{"pause", "--pr", "7", "--expect-revision", "0", "--key", "bad-reason", "--reason", "line\nbreak"},
		{"pause", "--pr", "7", "--expect-revision", "0", "--key", "bad-reason", "--reason", strings.Repeat("x", 257)},
		{"resume", "--pr", "7", "--expect-revision", "1", "--key", "unexpected-reason", "--reason", "hidden"},
		{"configure", "--automatic-hosted", "true", "--expect-revision", "0", "--key", "foreign", "--worker", "other"},
	} {
		var out, diag bytes.Buffer
		code := RunContext(t.Context(), append([]string{"--profiles-file", "/missing", "reviews", "scheduling"}, args...), nil, &out, &diag)
		if code != ExitUsage || out.Len() != 0 || !strings.Contains(diag.String(), "invalid_arguments") {
			t.Fatal("invalid scheduling input reached profile/transport", args, code, out.String(), diag.String())
		}
	}
	var out, diag bytes.Buffer
	if code := RunContext(t.Context(), []string{"--profiles-file", "/missing", "reviews", "scheduling", "--help"}, nil, &out, &diag); code != 0 || diag.Len() != 0 || !strings.Contains(out.String(), "docs/review-scheduling.md") || !strings.Contains(out.String(), "authorizes future") {
		t.Fatal("scheduling help not usable offline", code, out.String(), diag.String())
	}
}

func TestReviewSchedulingCLIOptionalAutomaticCLI(t *testing.T) {
	base := []string{"configure", "--automatic-hosted=false", "--expect-revision=0", "--key=cli-setting"}
	for _, value := range []string{"", "true", "false"} {
		args := append([]string{}, base...)
		if value != "" {
			args = append(args, "--automatic-cli="+value)
		}
		in, ok := parseReviewScheduling(args)
		if !ok || in.config.AutomaticHosted || (in.config.AutomaticCLI == nil) != (value == "") {
			t.Fatalf("lost optional CLI state: %q %+v", value, in)
		}
		if value != "" && *in.config.AutomaticCLI != (value == "true") {
			t.Fatal("wrong explicit CLI state")
		}
	}
	for _, extra := range [][]string{{"--automatic-cli=yes"}, {"--automatic-cli="}, {"--automatic-cli"}, {"--automatic-cli=true", "--automatic-cli=false"}} {
		if _, ok := parseReviewScheduling(append(append([]string{}, base...), extra...)); ok {
			t.Fatal("accepted invalid CLI flag", extra)
		}
	}
	if _, ok := parseReviewScheduling([]string{"configure", "--automatic-cli=true", "--expect-revision=0", "--key=missing-hosted"}); ok {
		t.Fatal("Hosted flag must remain explicit")
	}
}

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
