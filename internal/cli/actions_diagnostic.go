package cli

import (
	"github.com/benhook1013/FireController/internal/client"
	"strconv"
	"strings"
)

func parseActionsDiagnostic(args []string) (*client.ActionsDiagnosticOptions, bool) {
	options := &client.ActionsDiagnosticOptions{}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		flag, raw, assigned := strings.Cut(args[i], "=")
		if flag != "--age-hours" && flag != "--limit" || seen[flag] {
			return nil, false
		}
		seen[flag] = true
		if !assigned {
			i++
			if i >= len(args) {
				return nil, false
			}
			raw = args[i]
		}
		value, err := strconv.Atoi(raw)
		if err != nil || strconv.Itoa(value) != raw || value <= 0 {
			return nil, false
		}
		if flag == "--age-hours" {
			options.AgeHours = value
		} else {
			options.Limit = value
		}
	}
	return options, client.ValidateActionsDiagnosticOptions(*options) == nil
}
