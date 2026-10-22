package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"strings"

	"github.com/benhook1013/FireController/internal/offsite"
)

func runOffsiteCommand(ctx context.Context, command string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("fcd "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	config := flags.String("config", "", "private offsite configuration")
	var id, destination string
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
	var value any
	var err error
	switch command {
	case "backup-push":
		value, err = offsite.Push(ctx, *config)
	case "backup-fetch":
		value, err = offsite.Fetch(ctx, *config, id, destination)
	case "backup-offsite-status":
		value, err = offsite.ReadStatus(ctx, *config)
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
