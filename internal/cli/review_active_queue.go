package cli

import (
	"context"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
)

func parseReviewActiveQueue(args []string) (client.PageOptions, bool) {
	out := client.PageOptions{Limit: 5}
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
		if seen[flag] {
			return out, false
		}
		seen[flag] = true
		if flag == "--active" {
			if assigned {
				return out, false
			}
			continue
		}
		if flag != "--limit" && flag != "--cursor" {
			return out, false
		}
		if !assigned {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "-") {
				return out, false
			}
			value = args[i]
		}
		switch flag {
		case "--limit":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 100 {
				return out, false
			}
			out.Limit = n
		case "--cursor":
			if value == "" || len(value) > 4096 || !utf8.ValidString(value) || value != strings.TrimSpace(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
				return out, false
			}
			out.Cursor = value
		}
	}
	return out, seen["--active"]
}
func runReviewActiveQueue(ctx context.Context, service *client.Client, options client.PageOptions) (any, error) {
	return service.ReviewActiveQueue(ctx, options)
}
