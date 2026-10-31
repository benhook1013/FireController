package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/backuphealth"
	"github.com/benhook1013/FireController/internal/offsite"
)

func runOffsiteCommand(ctx context.Context, command string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("fcd "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	config := flags.String("config", "", "private offsite configuration")
	var id, destination, healthFile, healthLabel string
	var healthMaxAge time.Duration
	if command == "backup-offsite-status" {
		flags.StringVar(&healthFile, "health-file", "", "optional private redacted status output")
		flags.StringVar(&healthLabel, "health-label", "Offsite backup", "plain display label, maximum 80 bytes")
		flags.DurationVar(&healthMaxAge, "health-max-age", 3*time.Hour, "maximum completed snapshot age")
	}
	if command == "backup-fetch" {
		flags.StringVar(&id, "id", "", "selected remote backup identity")
		flags.StringVar(&destination, "destination", "", "new private destination root")
	}
	invalid := func() int {
		diagnostic(stderr, "configuration_invalid", "Provide --config and, for backup-fetch, a new --destination and optional --id (default: latest verified copy). Flags must occur once; no positional arguments are accepted.")
		return 2
	}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			return invalid()
		}
		name, _, assigned := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		if flags.Lookup(name) == nil || seen[name] {
			return invalid()
		}
		seen[name] = true
		if !assigned {
			i++
			if i >= len(args) {
				return invalid()
			}
		}
	}
	if flags.Parse(args) != nil || flags.NArg() != 0 || *config == "" || command == "backup-fetch" && destination == "" {
		return invalid()
	}
	if (seen["health-label"] || seen["health-max-age"]) && healthFile == "" {
		return invalid()
	}
	if healthFile != "" && (backuphealth.ValidateFiles([]string{healthFile}) != nil || !backuphealth.ValidLabel(healthLabel) || healthMaxAge < time.Minute || healthMaxAge > 30*24*time.Hour || healthMaxAge%time.Second != 0) {
		return invalid()
	}
	var value any
	var err error
	switch command {
	case "backup-push":
		value, err = offsite.Push(ctx, *config)
	case "backup-fetch":
		value, err = offsite.Fetch(ctx, *config, id, destination)
	case "backup-offsite-status":
		if healthFile != "" {
			value, err = offsite.ReadStatusAndWriteHealth(ctx, *config, healthFile, healthLabel, healthMaxAge)
		} else {
			value, err = offsite.ReadStatus(ctx, *config)
		}
	default:
		return invalid()
	}
	if err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	b, err := json.Marshal(value)
	if err != nil || len(b)+1 > 16<<10 {
		diagnostic(stderr, "offsite_output_failed", "The offsite result could not be reported; inspect local transfer status before retrying.")
		return 3
	}
	if n, err := stdout.Write(append(b, '\n')); err != nil || n != len(b)+1 {
		diagnostic(stderr, "offsite_output_failed", "The offsite result could not be written; inspect local transfer status before retrying.")
		return 3
	}
	return 0
}
