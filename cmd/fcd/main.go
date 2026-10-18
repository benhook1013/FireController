package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/benhook1013/FireController/internal/backup"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/historycollect"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/presentation"
	"github.com/benhook1013/FireController/internal/projectmap"
	"github.com/benhook1013/FireController/internal/publicsite"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewruntime"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/benhook1013/FireController/internal/server"
	"github.com/benhook1013/FireController/internal/webui"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) (exitCode int) {
	stderr = &daemonDiagnosticWriter{writer: stderr}
	if len(args) > 0 && args[0] == "restore-resume-providers" {
		return runProviderResumptionCommand(ctx, args[1:], stdout, stderr)
	}
	if len(args) > 0 && (args[0] == "backup" || args[0] == "backup-inspect" || args[0] == "backup-status" || args[0] == "restore" || args[0] == "restore-activate") {
		return runBackupCommand(ctx, args[0], args[1:], stdout, stderr)
	}
	flags := flag.NewFlagSet("fcd", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dataDir := flags.String("data-dir", "", "private local data directory (required)")
	listen := flags.String("listen", "127.0.0.1:8080", "loopback HTTP listen address")
	tokenFile := flags.String("github-token-file", "", "optional private GitHub credential file")
	historyEnabled := flags.Bool("history-collection", false, "enable explicitly requested GitHub history collection")
	reviewCatalog := flags.String("review-catalog", "", "optional private native CLI capability catalog")
	backupDestination := flags.String("backup-destination", "", "explicit managed backup root")
	backupInterval := flags.Duration("backup-interval", 0, "managed backup interval, 1m through 720h")
	backupKeep := flags.Int("backup-keep", 0, "managed verified bundles to retain, 1 through 1000")
	backupTimeout := flags.Duration("backup-timeout", 0, "managed attempt deadline, default 5m and maximum 24h")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || *dataDir == "" || !loopback(*listen) {
		diagnostic(stderr, "configuration_invalid", "Provide --data-dir and a loopback --listen address.")
		return 2
	}
	providedBackupFlags := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { providedBackupFlags[f.Name] = true })
	hasSchedule := providedBackupFlags["backup-destination"] || providedBackupFlags["backup-interval"] || providedBackupFlags["backup-keep"]
	if hasSchedule && (!providedBackupFlags["backup-destination"] || !providedBackupFlags["backup-interval"] || !providedBackupFlags["backup-keep"] || *backupDestination == "" || *backupInterval <= 0 || *backupKeep <= 0) || providedBackupFlags["backup-timeout"] && (!hasSchedule || *backupTimeout <= 0) {
		diagnostic(stderr, "configuration_invalid", "Configure --backup-destination, --backup-interval and --backup-keep together; --backup-timeout requires a schedule and a positive duration.")
		return 2
	}
	schedule, err := backup.ValidateScheduleConfig(*dataDir, backup.ScheduleConfig{Destination: *backupDestination, Interval: *backupInterval, Keep: *backupKeep, Timeout: *backupTimeout})
	if err != nil {
		diagnosticError(stderr, err)
		return 2
	}
	if err := backup.CheckDataDirectory(*dataDir); err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	// Existing restores must be fenced before reading any operator credential.
	// New installations still validate optional credentials before creating state.
	var store *core.Store
	defer func() {
		if store != nil {
			_ = store.Close()
		}
	}()
	_, existingErr := os.Lstat(filepath.Join(*dataDir, "firecontroller.sqlite"))
	if existingErr == nil {
		var err error
		store, err = openGuardedDaemonStore(ctx, *dataDir)
		if err != nil {
			diagnosticError(stderr, err)
			return 3
		}
	} else if !errors.Is(existingErr, os.ErrNotExist) {
		diagnostic(stderr, "storage_unavailable", "The private database file is unavailable or unsafe.")
		return 3
	}
	var recovery backup.RecoveryStatus
	if store != nil {
		recovery, err = backup.ReadRecoveryStatus(ctx, store)
		if err != nil {
			diagnosticError(stderr, err)
			return 3
		}
	}
	var catalog reviewruntime.Catalog
	var resolver server.RepositoryResolver
	var historyProvider historycollect.Provider
	var historyFingerprint string
	if !recovery.ProvidersHeld {
		catalog, err = reviewruntime.LoadCatalog(*reviewCatalog)
		if err != nil {
			diagnosticError(stderr, err)
			return 3
		}
		resolver, err = server.GitHubResolver(*tokenFile)
		if err != nil {
			diagnosticError(stderr, err)
			return 3
		}
		if *historyEnabled {
			historyProvider, historyFingerprint, err = server.GitHubHistoryReader(*tokenFile)
			if err != nil {
				diagnosticError(stderr, err)
				return 3
			}
		}
	}
	address := *listen
	if host, port, _ := net.SplitHostPort(address); host == "localhost" {
		address = net.JoinHostPort("127.0.0.1", port)
	}
	if store == nil {
		store, err = openGuardedDaemonStore(ctx, *dataDir)
		if err != nil {
			diagnosticError(stderr, err)
			return 3
		}
	}
	if err = jobs.EnsureSchema(ctx, store); err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	if err = projectmap.EnsureSchema(ctx, store); err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	if err = presentation.EnsureSchema(ctx, store); err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	if err = inbox.EnsureSchema(ctx, store); err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	if err = reviews.EnsureSchema(ctx, store); err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	if err = publicsite.EnsureSchema(ctx, store); err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	for _, ensure := range []func(context.Context, *core.Store) error{reviewexec.EnsureSchema, reviewexec.EnsureEvidenceSchema, reviewexec.EnsureAdjudicationSchema, reviewexec.EnsurePolicySchema} {
		if err = ensure(ctx, store); err != nil {
			diagnosticError(stderr, err)
			return 3
		}
	}
	if err = backup.EnsureManagedSchema(ctx, store); err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	if err = historycollect.EnsureSchema(ctx, store); err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	backupManager, err := backup.NewManager(store, *dataDir, schedule)
	if err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	resultWaitLifetime, cancelOperationWaits := context.WithCancel(ctx)
	defer cancelOperationWaits()
	execution, drivers, err := buildReviewExecution(store, *dataDir, catalog, reviewexec.WithResultWaitLifetime(resultWaitLifetime))
	if err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	collection, err := historycollect.New(store, historyProvider, historyFingerprint)
	if err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	mux := server.New(store, server.WithRepositoryResolver(resolver), server.WithBrowser(webui.Handler()))
	jobs.Register(mux, store)
	projectmap.Register(mux, store)
	presentation.Register(mux, store)
	inbox.Register(mux, store)
	reviews.Register(mux, store)
	publicsite.New(store, reviews.PublicSiteReviewFacts).Register(mux, ctx)
	execution.Register(mux)
	collection.Register(mux)
	server.RegisterMessageStream(mux, store, ctx)
	server.RegisterProjectStream(mux, store, ctx)
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", address)
	if err != nil {
		diagnostic(stderr, "listen_failed", "The loopback listener could not be started.")
		return 3
	}
	if !validateLoopbackListener(listener) {
		diagnostic(stderr, "configuration_invalid", "The bound listener must use a loopback TCP address.")
		return 2
	}
	defer listener.Close()
	reviewCtx, cancelReviews := context.WithCancel(ctx)
	var reviewDone <-chan error
	if !recovery.ProvidersHeld {
		reviewDone, err = startReviewWorkers(reviewCtx, execution, drivers, stderr)
	}
	defer func() {
		cancelReviews()
		if reviewDone == nil {
			return
		}
		select {
		case reviewErr := <-reviewDone:
			if reviewErr != nil {
				diagnosticError(stderr, reviewErr)
				exitCode = 3
			}
		case <-time.After(12 * time.Second):
			diagnostic(stderr, "shutdown_failed", "Native review workers did not settle within the shutdown budget; inspect retained evidence before restarting.")
			exitCode = 3
		}
	}()
	if err != nil {
		cancelReviews()
		diagnosticError(stderr, err)
		return 3
	}
	backupDone := startBackupWorker(reviewCtx, backupManager, schedule.Destination != "", func(err error) { diagnosticError(stderr, err) })
	defer func() {
		cancelReviews()
		if err := waitBackupWorker(backupDone, 12*time.Second); err != nil {
			diagnosticError(stderr, err)
			exitCode = 3
		}
	}()
	var collectionDone <-chan error
	if historyProvider != nil {
		collectionDone, err = collection.Run(reviewCtx)
		if err != nil {
			diagnosticError(stderr, err)
			return 3
		}
	}
	defer func() {
		cancelReviews()
		if collectionDone == nil {
			return
		}
		select {
		case err := <-collectionDone:
			if err != nil {
				diagnosticError(stderr, err)
				exitCode = 3
			}
		case <-time.After(12 * time.Second):
			diagnostic(stderr, "shutdown_failed", "History collection did not settle within the shutdown budget.")
			exitCode = 3
		}
	}()
	// Ensure result collectors detach before the worker and HTTP shutdown waits
	// on every exit path, while ordinary in-flight mutations keep draining.
	defer cancelOperationWaits()
	srv := newHTTPServer(ctx, mux)
	_ = json.NewEncoder(stdout).Encode(map[string]string{"event": "listening", "address": listener.Addr().String(), "instance_id": store.InstanceID()})
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			diagnostic(stderr, "serve_failed", "The HTTP service stopped unexpectedly.")
			return 3
		}
	case <-ctx.Done():
		cancelOperationWaits()
		if err = shutdownHTTPServer(srv, 5*time.Second); err != nil {
			diagnostic(stderr, "shutdown_failed", "The HTTP service required forced shutdown.")
			return 3
		}
		<-done
	case backupErr := <-backupDone:
		cancelOperationWaits()
		cancelReviews()
		shutdownErr := shutdownHTTPServer(srv, 5*time.Second)
		<-done
		if backupErr != nil {
			diagnosticError(stderr, backupErr)
			return 3
		}
		if shutdownErr != nil || ctx.Err() == nil {
			diagnostic(stderr, "backup_worker_stopped", "The managed backup worker stopped unexpectedly.")
			return 3
		}
	case reviewErr := <-reviewDone:
		cancelOperationWaits()
		cancelReviews()
		shutdownErr := shutdownHTTPServer(srv, 5*time.Second)
		<-done
		if reviewErr != nil {
			diagnosticError(stderr, reviewErr)
			return 3
		}
		if shutdownErr != nil || ctx.Err() == nil {
			diagnostic(stderr, "review_execution_stopped", "The native review supervisor stopped unexpectedly.")
			return 3
		}
	case collectionErr := <-collectionDone:
		cancelOperationWaits()
		cancelReviews()
		shutdownErr := shutdownHTTPServer(srv, 5*time.Second)
		<-done
		if collectionErr != nil {
			diagnosticError(stderr, collectionErr)
			return 3
		}
		if shutdownErr != nil || ctx.Err() == nil {
			diagnostic(stderr, "history_collection_stopped", "The history collection worker stopped unexpectedly.")
			return 3
		}
	}
	return 0
}

func validateLoopbackListener(listener net.Listener) bool {
	address, ok := listener.Addr().(*net.TCPAddr)
	if ok && address != nil && address.IP.IsLoopback() {
		return true
	}
	_ = listener.Close()
	return false
}

func newHTTPServer(signalContext context.Context, handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(signalContext) }, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
}

func shutdownHTTPServer(srv *http.Server, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
		return err
	}
	return nil
}

func loopback(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return false
	}
	return host == "localhost" || net.ParseIP(host).IsLoopback()
}
func diagnostic(w io.Writer, code, message string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func diagnosticError(w io.Writer, err error) {
	var failure *core.Error
	if errors.As(err, &failure) {
		diagnostic(w, failure.Code, failure.Message)
	} else {
		diagnostic(w, "internal_error", "The service could not be started.")
	}
}
