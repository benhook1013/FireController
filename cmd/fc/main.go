package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/benhook1013/FireController/internal/cli"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, found := strings.Cut(entry, "=")
		if found {
			env[key] = value
		}
	}
	return cli.RunContext(ctx, os.Args[1:], env, os.Stdout, os.Stderr)
}
