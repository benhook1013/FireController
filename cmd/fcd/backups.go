package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/backup"
	"github.com/benhook1013/FireController/internal/core"
)

// Local operator actions deliberately bypass daemon/catalog/worker startup.
func runBackupCommand(ctx context.Context, command string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("fcd "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard) // Parser errors can reflect private argument values.
	var dataDir, destination, bundle string
	switch command {
	case "backup-status":
		flags.StringVar(&dataDir, "data-dir", "", "existing private source directory")
	case "backup":
		flags.StringVar(&dataDir, "data-dir", "", "existing private source directory")
		flags.StringVar(&destination, "destination", "", "private backup root")
	case "backup-inspect":
		flags.StringVar(&bundle, "bundle", "", "completed private bundle directory")
	case "restore":
		flags.StringVar(&bundle, "bundle", "", "completed private bundle directory")
		flags.StringVar(&dataDir, "data-dir", "", "new private inactive destination")
	}
	timeout := flags.String("timeout", "5m", "positive local operation deadline, at most 24h")
	invalid := func() int {
		diagnostic(stderr, "configuration_invalid", "Use the selected local backup command's required paths and one positive --timeout of at most 24h; duplicate flags and positional arguments are not accepted.")
		return 2
	}
	// flag.FlagSet permits repeats. Reject them before parsing or filesystem work,
	// while preserving ordinary string values that themselves begin with a dash.
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") || args[i] == "--" {
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
	if flags.Parse(args) != nil || flags.NArg() != 0 || command == "backup" && (strings.TrimSpace(dataDir) == "" || strings.TrimSpace(destination) == "") || command == "backup-status" && strings.TrimSpace(dataDir) == "" || command == "backup-inspect" && strings.TrimSpace(bundle) == "" || command == "restore" && (strings.TrimSpace(bundle) == "" || strings.TrimSpace(dataDir) == "") {
		return invalid()
	}
	duration, err := time.ParseDuration(*timeout)
	if err != nil || duration <= 0 || duration > 24*time.Hour {
		return invalid()
	}
	operation, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	var summary any
	switch command {
	case "backup":
		summary, err = backup.CreateFromDirectory(operation, dataDir, destination)
	case "backup-status":
		if err = backup.CheckDataDirectory(dataDir); err == nil {
			summary, err = backup.ReadStatus(operation, dataDir)
		}
	case "backup-inspect":
		summary, err = backup.Inspect(operation, bundle)
	case "restore":
		summary, err = backup.Restore(operation, bundle, dataDir)
	}
	if err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	data, err := json.Marshal(summary)
	resultLimit := 4096
	if command == "backup-status" {
		resultLimit = 16 << 10
	}
	if err != nil || len(data)+1 > resultLimit {
		diagnostic(stderr, "backup_unavailable", "The local backup result could not be reported safely; inspect the selected destination before retrying.")
		return 3
	}
	data = append(data, '\n')
	if n, err := stdout.Write(data); err != nil || n != len(data) {
		diagnostic(stderr, "output_failed", "The local backup result could not be written; inspect the selected destination before retrying.")
		return 3
	}
	return 0
}

func openGuardedDaemonStore(ctx context.Context, dataDir string) (*core.Store, error) {
	store, err := core.Open(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	if err = backup.StartupGuard(ctx, store); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}
