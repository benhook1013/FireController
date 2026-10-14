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
	"github.com/benhook1013/FireController/internal/server"
	"github.com/benhook1013/FireController/internal/webui"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("fcd", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dataDir := flags.String("data-dir", "", "private local data directory (required)")
	listen := flags.String("listen", "127.0.0.1:8080", "loopback HTTP listen address")
	tokenFile := flags.String("github-token-file", "", "optional private GitHub credential file")
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
	store, err := core.Open(ctx, *dataDir)
	if err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	defer store.Close()
	resolver, err := server.GitHubResolver(*tokenFile)
	if err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	if err = jobs.EnsureSchema(ctx, store); err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	if err = inbox.EnsureSchema(ctx, store); err != nil {
		diagnosticError(stderr, err)
		return 3
	}
	mux := server.New(store, server.WithRepositoryResolver(resolver), server.WithBrowser(webui.Handler()))
	jobs.Register(mux, store)
	inbox.Register(mux, store)
	server.RegisterMessageStream(mux, store, ctx)
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		diagnostic(stderr, "listen_failed", "The loopback listener could not be started.")
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
	}
	return 0
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
