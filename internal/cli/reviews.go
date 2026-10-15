package cli

import (
	"context"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
)

func parseReview(call invocation, args []string) (invocation, bool) {
	list := call.subcommand == "queue" || call.subcommand == "runs"
	switch call.subcommand {
	case "queue", "pull", "runs", "run":
	default:
		return call, false
	}
	if list {
		call.limit = 5
	}
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if !strings.HasPrefix(argument, "-") {
			call.position = append(call.position, argument)
			continue
		}
		flag, value, assigned := strings.Cut(argument, "=")
		if seen[flag] || !(list && (flag == "--limit" || flag == "--cursor") || call.subcommand != "queue" && flag == "--snapshot") {
			return call, false
		}
		seen[flag] = true
		if !assigned {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "-") {
				return call, false
			}
			value = args[i]
		}
		switch flag {
		case "--limit":
			limit, err := strconv.Atoi(value)
			if err != nil || limit < 1 || limit > 100 {
				return call, false
			}
			call.limit = limit
		case "--cursor":
			if value == "" || len(value) > 4096 || !utf8.ValidString(value) || value != strings.TrimSpace(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
				return call, false
			}
			call.cursor = value
		case "--snapshot":
			if !reviewID(value, "review_snapshot_") {
				return call, false
			}
			call.snapshot = value
		}
	}
	if call.subcommand == "queue" {
		return call, len(call.position) == 0
	}
	if len(call.position) != 1 {
		return call, false
	}
	if call.subcommand == "run" {
		return call, reviewID(call.position[0], "review_run_")
	}
	number, err := strconv.ParseInt(call.position[0], 10, 64)
	if err != nil || number < 1 || number > 2147483647 || strconv.FormatInt(number, 10) != call.position[0] {
		return call, false
	}
	call.pullNumber = number
	return call, true
}

func reviewID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+32 {
		return false
	}
	for _, c := range value[len(prefix):] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func executeReview(ctx context.Context, service *client.Client, call invocation) (any, error) {
	switch call.subcommand {
	case "queue":
		return service.ReviewQueue(ctx, client.PageOptions{Limit: call.limit, Cursor: call.cursor})
	case "pull":
		return service.ReviewPull(ctx, call.pullNumber, call.snapshot)
	case "runs":
		return service.ReviewRuns(ctx, call.pullNumber, client.ReviewRunOptions{PageOptions: client.PageOptions{Limit: call.limit, Cursor: call.cursor}, Snapshot: call.snapshot})
	default: // parseReview admits only the explicit run-detail command here.
		return service.ReviewRun(ctx, call.position[0], call.snapshot)
	}
}
