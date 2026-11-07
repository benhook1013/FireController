package cli

import (
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/client"
)

func parseOperationList(call invocation, args []string) (invocation, bool) {
	o := client.ReviewOperationsOptions{PageOptions: client.PageOptions{Limit: 5}, Status: "pending"}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
		if seen[flag] {
			return call, false
		}
		seen[flag] = true
		if flag == "--all" {
			if assigned {
				return call, false
			}
			o.Status = "all"
			continue
		}
		if flag != "--pr" && flag != "--channel" && flag != "--limit" && flag != "--cursor" {
			return call, false
		}
		if !assigned {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "-") {
				return call, false
			}
			value = args[i]
		}
		if value == "" {
			return call, false
		}
		switch flag {
		case "--pr":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 1 || strconv.FormatInt(n, 10) != value {
				return call, false
			}
			o.PullNumber = n
		case "--channel":
			o.Channel = value
		case "--limit":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || strconv.Itoa(n) != value {
				return call, false
			}
			o.Limit = n
		case "--cursor":
			o.Cursor = value
		}
	}
	call.reviewOperations = o
	return call, client.ValidateReviewOperationsOptions(o) == nil
}
