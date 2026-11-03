package cli

import (
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/client"
)

func parseReviewActivity(args []string) (client.ReviewActivityOptions, bool) {
	var options client.ReviewActivityOptions
	values := map[string]string{}
	for index := 0; index < len(args); index++ {
		flag, value, assigned := strings.Cut(args[index], "=")
		if flag != "--hours" && flag != "--since" && flag != "--until" && flag != "--timezone" && flag != "--pr" {
			return options, false
		}
		if _, duplicate := values[flag]; duplicate {
			return options, false
		}
		if !assigned {
			index++
			if index >= len(args) {
				return options, false
			}
			value = args[index]
		}
		if value == "" {
			return options, false
		}
		values[flag] = value
	}
	for flag, value := range values {
		switch flag {
		case "--hours":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || strconv.Itoa(n) != value {
				return options, false
			}
			options.Hours = n
		case "--pr":
			n, err := strconv.ParseInt(value, 10, 32)
			if err != nil || n < 1 || strconv.FormatInt(n, 10) != value {
				return options, false
			}
			options.PullNumber = n
		case "--since":
			options.Since = value
		case "--until":
			options.Until = value
		case "--timezone":
			options.Timezone = value
		}
	}
	return options, client.ValidateReviewActivityOptions(options) == nil
}
