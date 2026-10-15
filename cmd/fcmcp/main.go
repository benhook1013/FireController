package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/benhook1013/FireController/internal/mcpadapter"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		env[name] = value
	}
	return mcpadapter.Run(ctx, os.Args[1:], env, os.Stdin, os.Stdout, os.Stderr)
}
