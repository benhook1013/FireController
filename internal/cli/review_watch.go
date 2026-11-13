package cli

import (
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/client"
)

type reviewWatchInvocation struct {
	options client.ReviewWatchOptions
	seconds int
}

func parseReviewWatch(args []string) (*reviewWatchInvocation, bool) {
	out := &reviewWatchInvocation{options: client.ReviewWatchOptions{Limit: 20}, seconds: client.ReviewWatchDefaultSeconds}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		flag, raw, set := strings.Cut(args[i], "=")
		if seen[flag] {
			return nil, false
		}
		seen[flag] = true
		if flag != "--pr" && flag != "--channel" && flag != "--limit" && flag != "--after" && flag != "--wait-seconds" {
			return nil, false
		}
		if !set {
			i++
			if i >= len(args) {
				return nil, false
			}
			raw = args[i]
		}
		if raw == "" {
			return nil, false
		}
		switch flag {
		case "--pr":
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 1 || strconv.FormatInt(n, 10) != raw {
				return nil, false
			}
			out.options.PullNumber = n
		case "--channel":
			out.options.Channel = raw
		case "--after":
			out.options.After = raw
		default:
			n, err := strconv.Atoi(raw)
			if err != nil || strconv.Itoa(n) != raw {
				return nil, false
			}
			if flag == "--limit" {
				out.options.Limit = n
			} else {
				out.seconds = n
			}
		}
	}
	return out, client.ValidateReviewWatch(out.options, out.seconds) == nil
}
