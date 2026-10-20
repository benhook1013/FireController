package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"math"
	"path/filepath"
	"time"

	"github.com/benhook1013/FireController/internal/strictjson"
	"github.com/benhook1013/FireController/tools/legacyimport/internal/legacy"
)

const prepareMessagesUsage = `Usage: legacyimport prepare-messages --snapshot ABSOLUTE_PATH --selection ABSOLUTE_JSON --output ABSOLUTE_JSON

Prepare explicitly selected historical messages with mapped authors/recipients
and selected or already-imported parents. Association policy must be archive.
Private output is a native import batch; no service or delivery is called.
Optional --timeout bounds preparation (default 30s, maximum 5m).
Explicit positive inspection limits (omitted flags retain inspect defaults):
  --max-database-bytes N          Snapshot size limit
  --max-rows-per-table N          Per-table row limit
  --max-total-rows N              Total row limit
  --max-cell-bytes N              Individual cell size limit
  --max-total-cell-bytes N        Total cell byte limit
`

func runPrepareMessages(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--help" {
		if _, err := io.WriteString(stdout, prepareMessagesUsage); err != nil {
			return 3
		}
		return 0
	}
	flags := flag.NewFlagSet("legacyimport prepare-messages", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	invalid := func() int {
		diagnostic(stderr, "arguments_invalid", "Use prepare-messages with absolute snapshot, selection and output paths, positive limits and a timeout of at most five minutes.")
		return 2
	}
	snapshot := flags.String("snapshot", "", "")
	selectionPath := flags.String("selection", "", "")
	output := flags.String("output", "", "")
	budget := flags.Duration("timeout", 30*time.Second, "")
	var limits legacy.Limits
	flags.Int64Var(&limits.MaxDatabaseBytes, "max-database-bytes", 0, "snapshot size limit")
	flags.Int64Var(&limits.MaxRowsPerTable, "max-rows-per-table", 0, "per-table row limit")
	flags.Int64Var(&limits.MaxTotalRows, "max-total-rows", 0, "total row limit")
	flags.Int64Var(&limits.MaxCellBytes, "max-cell-bytes", 0, "individual cell limit")
	flags.Int64Var(&limits.MaxTotalCellBytes, "max-total-cell-bytes", 0, "total cell limit")
	if !uniqueFlags(flags, args) || !filepath.IsAbs(*snapshot) || !filepath.IsAbs(*selectionPath) || !filepath.IsAbs(*output) || *budget <= 0 || *budget > 5*time.Minute {
		return invalid()
	}
	provided := map[string]int64{
		"max-database-bytes":   limits.MaxDatabaseBytes,
		"max-rows-per-table":   limits.MaxRowsPerTable,
		"max-total-rows":       limits.MaxTotalRows,
		"max-cell-bytes":       limits.MaxCellBytes,
		"max-total-cell-bytes": limits.MaxTotalCellBytes,
	}
	invalidLimit := false
	flags.Visit(func(f *flag.Flag) {
		if value, ok := provided[f.Name]; ok && (value <= 0 || value == math.MaxInt64) {
			invalidLimit = true
		}
	})
	if invalidLimit {
		return invalid()
	}
	request, cancel := context.WithTimeout(ctx, *budget)
	defer cancel()
	data, err := privateFile(*selectionPath, 64<<10)
	var selection legacy.MessageSelection
	if err != nil || strictjson.Decode(data, &selection) != nil {
		diagnostic(stderr, "selection_invalid", "Supply a private regular strict JSON selection within 64 KiB.")
		return 3
	}
	batch, summary, err := legacy.PrepareMessages(request, *snapshot, limits, selection)
	if err != nil {
		if errors.Is(request.Err(), context.Canceled) {
			diagnostic(stderr, "preparation_canceled", "Message preparation was canceled.")
			return 130
		}
		if errors.Is(request.Err(), context.DeadlineExceeded) {
			diagnostic(stderr, "preparation_timed_out", "Message preparation exceeded its time budget.")
			return 3
		}
		var safe *legacy.Error
		if errors.As(err, &safe) && safe != nil {
			diagnostic(stderr, safe.Code, safe.Message)
		} else {
			diagnostic(stderr, "preparation_failed", "The selected source could not be prepared.")
		}
		return 3
	}
	body, err := jsonBytes(batch)
	if err != nil {
		diagnostic(stderr, "preparation_failed", "The native batch could not be encoded.")
		return 3
	}
	if request.Err() != nil {
		diagnostic(stderr, "preparation_interrupted", "Message preparation was interrupted before publication.")
		return 3
	}
	if err = publishPrepared(*output, body); err != nil {
		diagnostic(stderr, "output_unavailable", "The private output could not be published or already differs. Inspect the selected output; retry with the same selection and output path.")
		return 3
	}
	encoded, err := jsonBytes(summary)
	if err != nil {
		return 3
	}
	if n, e := stdout.Write(encoded); e != nil || n != len(encoded) {
		diagnostic(stderr, "output_unavailable", "The prepared file may be available; inspect the output or retry the same selection and path.")
		return 3
	}
	return 0
}
