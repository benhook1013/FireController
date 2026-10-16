package cli

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

func parseMessageWait(call invocation, args []string) (invocation, bool) {
	call.limit, call.waitSeconds = 20, 25
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
			if value == "" || len(value) > 4096 || !utf8.ValidString(value) || value != strings.TrimSpace(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
				return call, false
			}
			call.cursor = value
		default:
			n, err := strconv.Atoi(value)
			maximum := 100
			if flag == "--wait-seconds" {
				maximum = 60
			}
			if err != nil || n < 1 || n > maximum {
				return call, false
			}
			if flag == "--limit" {
				call.limit = n
			} else {
				call.waitSeconds = n
			}
		}
	}
	return call, seen["--cursor"]
}
