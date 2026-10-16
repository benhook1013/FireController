package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/backup"
)

// This offline operator action never enters daemon or provider startup.
func runProviderResumptionCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("fcd restore-resume-providers", flag.ContinueOnError)
	flags.SetOutput(io.Discard) // Flag errors may contain private argument values.
	var dataDir string
	var request backup.ProviderResumptionRequest
	flags.StringVar(&dataDir, "data-dir", "", "existing coordination-active restore directory")
	flags.StringVar(&request.ExpectedInstanceID, "expect-instance", "", "exact restored instance identity")
	flags.StringVar(&request.FencedPredecessorID, "fenced-predecessor", "", "exact externally fenced predecessor")
	flags.BoolVar(&request.ProviderEffectsSettled, "provider-effects-settled", false, "operator attestation that external effects are settled")
	flags.BoolVar(&request.AbandonRetainedWork, "abandon-retained-work", false, "explicitly abandon retained pending work")
	flags.StringVar(&request.Reason, "reason", "", "private reason, at most 256 UTF-8 bytes")
	flags.StringVar(&request.EvidenceReference, "evidence-reference", "", "private evidence reference, at most 256 UTF-8 bytes")
	timeout := flags.String("timeout", "5m", "positive local deadline, at most 24h")
	invalid := func() int {
		diagnostic(stderr, "configuration_invalid", "Provide the existing restore directory, exact instance and fenced predecessor, both true attestations, a nonblank reason and evidence reference of at most 256 UTF-8 bytes each, and one positive timeout of at most 24h; duplicate flags and positional arguments are not accepted.")
		return 2
	}
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if !strings.HasPrefix(argument, "-") || argument == "--" || strings.HasPrefix(argument, "---") {
			return invalid()
		}
		name, value, assigned := strings.Cut(strings.TrimLeft(argument, "-"), "=")
		if flags.Lookup(name) == nil || seen[name] {
			return invalid()
		}
		seen[name] = true
		if name == "provider-effects-settled" || name == "abandon-retained-work" {
			if assigned && value != "true" {
				return invalid()
			}
			continue
		}
		if !assigned {
			i++
			if i >= len(args) {
				return invalid()
			}
			value = args[i]
		}
		if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return invalid()
		}
	}
	if flags.Parse(args) != nil || flags.NArg() != 0 || dataDir == "" || !resumptionInstanceID(request.ExpectedInstanceID) || !resumptionInstanceID(request.FencedPredecessorID) || request.ExpectedInstanceID == request.FencedPredecessorID || !request.ProviderEffectsSettled || !request.AbandonRetainedWork || request.Reason == "" || len(request.Reason) > 256 || request.EvidenceReference == "" || len(request.EvidenceReference) > 256 {
		return invalid()
	}
	duration, err := time.ParseDuration(*timeout)
	if err != nil || duration <= 0 || duration > 24*time.Hour {
		return invalid()
	}
	operation, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	summary, err := backup.ResumeProviders(operation, dataDir, request)
	if err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	data, err := json.Marshal(summary)
	if err != nil || len(data)+1 > 4096 {
		return providerResumptionOutputFailure(stderr)
	}
	data = append(data, '\n')
	if n, err := stdout.Write(data); err != nil || n != len(data) {
		return providerResumptionOutputFailure(stderr)
	}
	return 0
}

func resumptionInstanceID(value string) bool {
	if len(value) != len("instance_")+32 || !strings.HasPrefix(value, "instance_") {
		return false
	}
	for _, c := range value[len("instance_"):] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func providerResumptionOutputFailure(stderr io.Writer) int {
	_ = json.NewEncoder(stderr).Encode(map[string]any{"error": map[string]any{
		"code": "output_failed", "message": "The local provider resumption result could not be reported.",
		"action": "Retry the exact same command and arguments; do not change the instance, predecessor or settlement assertions.", "outcome_unknown": true,
	}})
	return 3
}
