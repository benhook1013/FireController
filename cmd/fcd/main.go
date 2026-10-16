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
	"strconv"
	"syscall"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
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
	flags := flag.NewFlagSet("fcd", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dataDir := flags.String("data-dir", "", "private local data directory (required)")
	listen := flags.String("listen", "127.0.0.1:8080", "loopback HTTP listen address")
	tokenFile := flags.String("github-token-file", "", "optional private GitHub credential file")
	reviewCatalog := flags.String("review-catalog", "", "optional private native CLI capability catalog")
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
	catalog, err := reviewruntime.LoadCatalog(*reviewCatalog)
	if err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	address := *listen
	if host, port, _ := net.SplitHostPort(address); host == "localhost" {
		address = net.JoinHostPort("127.0.0.1", port)
	}
	resolver, err := server.GitHubResolver(*tokenFile)
	if err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	store, err := core.Open(ctx, *dataDir)
	if err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	defer store.Close()
	if err = jobs.EnsureSchema(ctx, store); err != nil {
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
	for _, ensure := range []func(context.Context, *core.Store) error{reviewexec.EnsureSchema, reviewexec.EnsureEvidenceSchema, reviewexec.EnsureAdjudicationSchema, reviewexec.EnsurePolicySchema} {
		if err = ensure(ctx, store); err != nil {
			diagnosticError(stderr, err)
			return 3
		}
	}
	execution, drivers, err := buildReviewExecution(store, *dataDir, catalog)
	if err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	mux := server.New(store, server.WithRepositoryResolver(resolver), server.WithBrowser(webui.Handler()))
	jobs.Register(mux, store)
	inbox.Register(mux, store)
	reviews.Register(mux, store)
	execution.Register(mux)
	server.RegisterMessageStream(mux, store, ctx)
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
	reviewDone, err := startReviewWorkers(reviewCtx, execution, drivers, stderr)
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
		if err = shutdownHTTPServer(srv, 5*time.Second); err != nil {
			diagnostic(stderr, "shutdown_failed", "The HTTP service required forced shutdown.")
			return 3
		}
		<-done
	case reviewErr := <-reviewDone:
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
