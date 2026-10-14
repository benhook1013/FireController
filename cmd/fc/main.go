package main

import (
	"os"
	"strings"

	"github.com/benhook1013/FireController/internal/cli"
)

func main() {
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, found := strings.Cut(entry, "=")
		if found {
			env[key] = value
		}
	}
	os.Exit(cli.Run(os.Args[1:], env, os.Stdout, os.Stderr))
}
