package cli

import (
	"bytes"
	"strings"
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
