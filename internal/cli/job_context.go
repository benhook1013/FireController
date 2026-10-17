package cli

import (
	"strconv"
	"strings"
)

func parseJobContext(call invocation, args []string) (invocation, bool) {
	call.limit = 20
	seen := false
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			call.position = append(call.position, args[i])
			continue
		}
		flag, value, assigned := strings.Cut(args[i], "=")
		if flag != "--limit" || seen {
			return call, false
		}
		seen = true
		if !assigned {
			i++
			if i >= len(args) {
				return call, false
			}
			value = args[i]
		}
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 100 {
			return call, false
		}
		call.limit = limit
	}
	return call, len(call.position) == 1 && validResourceID(call.position[0])
}
