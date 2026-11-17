package cli

import (
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/client"
)

func parseActionsRunDiagnostic(args []string) (*client.ActionsRunDiagnosticOptions, bool) {
	o := &client.ActionsRunDiagnosticOptions{}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		flag, raw, assigned := strings.Cut(args[i], "=")
		if (flag != "--run" && flag != "--attempt" && flag != "--limit") || seen[flag] {
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
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 || strconv.FormatInt(value, 10) != raw {
			return nil, false
		}
		switch flag {
		case "--run":
			o.RunID = value
		case "--attempt":
			o.RunAttempt = value
		case "--limit":
			if value > 20 {
				return nil, false
			}
			o.Limit = int(value)
		}
	}
	return o, client.ValidateActionsRunDiagnosticOptions(*o) == nil
}
