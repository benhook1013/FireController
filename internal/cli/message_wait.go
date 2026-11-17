package cli

import (
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/client"
)

func parseMessageWait(call invocation, args []string) (invocation, bool) {
	call.limit, call.waitSeconds = client.MessageWaitDefaultLimit, client.MessageWaitDefaultSeconds
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
		if (flag != "--cursor" && flag != "--limit" && flag != "--wait-seconds") || seen[flag] {
			return call, false
		}
		seen[flag] = true
		if !assigned {
			i++
			if i >= len(args) {
				return call, false
			}
			value = args[i]
		}
		switch flag {
		case "--cursor":
			call.cursor = value
		default:
			n, err := strconv.Atoi(value)
			if err != nil {
				return call, false
			}
			if flag == "--limit" {
				call.limit = n
			} else {
				call.waitSeconds = n
			}
		}
	}
	return call, seen["--cursor"] && client.ValidateMessageWait(call.cursor, call.limit, call.waitSeconds) == nil
}
