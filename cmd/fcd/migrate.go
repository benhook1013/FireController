package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/backup"
	"github.com/benhook1013/FireController/internal/coordimport"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/historycollect"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/opvars"
	"github.com/benhook1013/FireController/internal/presentation"
	"github.com/benhook1013/FireController/internal/projectmap"
	"github.com/benhook1013/FireController/internal/publicsite"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/benhook1013/FireController/internal/tracking"
)

// ensureDaemonSchemas applies every storage migration the daemon needs, in
// startup order. The daemon and `fcd migrate` share it so a deploy can rehearse
// exactly the migrations the new binary will run.
func ensureDaemonSchemas(ctx context.Context, store *core.Store) error {
	steps := []func(context.Context, *core.Store) error{
		jobs.EnsureSchema, projectmap.EnsureSchema, tracking.EnsureSchema, presentation.EnsureSchema,
		opvars.EnsureSchema, inbox.EnsureSchema, coordimport.EnsureSchema, reviews.EnsureSchema,
		publicsite.EnsureSchema, reviewexec.EnsureSchema, reviewexec.EnsureEvidenceSchema,
		reviewexec.EnsureAdjudicationSchema, reviewexec.EnsurePolicySchema,
		reviewexec.SeedNotificationFollowers, backup.EnsureManagedSchema, historycollect.EnsureSchema,
	}
	for _, step := range steps {
		if err := step(ctx, store); err != nil {
			return err
		}
	}
	return nil
}

// runMigrateCommand migrates a data directory and exits without starting any
// worker, listener or provider client. Deploys run it on a copy of the
// pre-deploy snapshot before replacing the daemon.
func runMigrateCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("fcd migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dataDir := flags.String("data-dir", "", "private data directory to migrate (required)")
	timeout := flags.Duration("timeout", 10*time.Minute, "maximum migration time")
	if flags.Parse(args) != nil || flags.NArg() != 0 || strings.TrimSpace(*dataDir) == "" || *timeout <= 0 {
		diagnostic(stderr, "configuration_invalid", "Use: fcd migrate --data-dir DIR [--timeout DURATION].")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	store, err := openGuardedDaemonStore(ctx, *dataDir, core.WithSchemaMigrationTimeout(*timeout))
	if err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	defer store.Close()
	if err = ensureDaemonSchemas(ctx, store); err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	var check string
	if err = store.DB().QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&check); err != nil || check != "ok" {
		diagnostic(stderr, "storage_unavailable", "The migrated database failed its integrity check.")
		return 3
	}
	_ = json.NewEncoder(stdout).Encode(map[string]string{"state": "migrated", "quick_check": check})
	return 0
}
