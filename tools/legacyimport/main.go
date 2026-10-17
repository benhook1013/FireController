// legacyimport is an optional migration utility, separate from the service.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/benhook1013/FireController/tools/legacyimport/internal/legacy"
)

const usage = `Usage: legacyimport inspect --snapshot ABSOLUTE_PATH [options]

The prepare-jobs and prepare-messages commands prepare explicitly selected history;
use either command with --help for its explicit file selection.

Inspect a sealed legacy-controller SQLite snapshot. Output is compact JSON;
the command does not modify the snapshot or contact a service.

Options:
  --timeout DURATION             Total inspection budget (default 30s; maximum 5m)
  --max-database-bytes N          Override the snapshot size limit
  --max-rows-per-table N          Override the per-table row limit
  --max-total-rows N              Override the total row limit
  --max-cell-bytes N              Override the individual cell size limit
  --max-total-cell-bytes N        Override the total cell byte limit
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func diagnostic(w io.Writer, code, message string) {
	_ = json.NewEncoder(w).Encode(struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message}})
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "prepare-messages" {
		return runPrepareMessages(ctx, args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "prepare-jobs" {
		return runPrepare(ctx, args[1:], stdout, stderr)
	}
	if len(args) == 1 && args[0] == "--help" || len(args) == 2 && args[0] == "inspect" && args[1] == "--help" {
		if _, err := io.WriteString(stdout, usage); err != nil {
			return 3
		}
		return 0
	}
	invalid := func() int {
		diagnostic(stderr, "arguments_invalid", "Use inspect --snapshot ABSOLUTE_PATH with supported positive limits and a timeout of at most five minutes.")
		return 2
	}
	if len(args) < 1 || args[0] != "inspect" {
		return invalid()
	}
	flags := flag.NewFlagSet("legacyimport inspect", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var snapshot string
	var limits legacy.Limits
	var budget time.Duration
	flags.StringVar(&snapshot, "snapshot", "", "sealed private snapshot")
	flags.DurationVar(&budget, "timeout", 30*time.Second, "total inspection budget")
	flags.Int64Var(&limits.MaxDatabaseBytes, "max-database-bytes", 0, "snapshot size limit")
	flags.Int64Var(&limits.MaxRowsPerTable, "max-rows-per-table", 0, "per-table row limit")
	flags.Int64Var(&limits.MaxTotalRows, "max-total-rows", 0, "total row limit")
	flags.Int64Var(&limits.MaxCellBytes, "max-cell-bytes", 0, "individual cell limit")
	flags.Int64Var(&limits.MaxTotalCellBytes, "max-total-cell-bytes", 0, "total cell limit")
	// The standard flag package accepts duplicate flags and can echo private
	// arguments in diagnostics. Admit one explicit value for each known flag.
	if !uniqueFlags(flags, args[1:]) {
		return invalid()
	}
	seen := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if !filepath.IsAbs(snapshot) || strings.TrimSpace(snapshot) != snapshot || strings.IndexFunc(snapshot, unicode.IsControl) >= 0 || budget <= 0 || budget > 5*time.Minute {
		return invalid()
	}
	provided := map[string]int64{
		"max-database-bytes":   limits.MaxDatabaseBytes,
		"max-rows-per-table":   limits.MaxRowsPerTable,
		"max-total-rows":       limits.MaxTotalRows,
		"max-cell-bytes":       limits.MaxCellBytes,
		"max-total-cell-bytes": limits.MaxTotalCellBytes,
	}
	for name, value := range provided {
		if seen[name] && value <= 0 {
			return invalid()
		}
	}
	request, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	inspection, err := legacy.InspectSnapshot(request, snapshot, limits)
	if err != nil {
		if errors.Is(request.Err(), context.Canceled) {
			diagnostic(stderr, "inspection_canceled", "Snapshot inspection was canceled.")
			return 130
		}
		if errors.Is(request.Err(), context.DeadlineExceeded) {
			diagnostic(stderr, "inspection_timed_out", "Snapshot inspection exceeded its time budget.")
			return 3
		}
		var safe *legacy.Error
		if errors.As(err, &safe) && safe != nil {
			diagnostic(stderr, safe.Code, safe.Message)
		} else {
			diagnostic(stderr, "snapshot_unavailable", "The selected snapshot could not be inspected.")
		}
		return 3
	}
	data, err := json.Marshal(inspection)
	if err != nil {
		diagnostic(stderr, "output_unavailable", "The inspection result could not be encoded.")
		return 3
	}
	data = append(data, '\n')
	if n, err := stdout.Write(data); err != nil || n != len(data) {
		diagnostic(stderr, "output_unavailable", "The inspection result could not be written.")
		return 3
	}
	return 0
}
