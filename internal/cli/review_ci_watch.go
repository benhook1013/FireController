package cli

import (
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/client"
)

type ciWatchInvocation struct {
	targets []client.CIWatchTarget
	seconds int
}

func parseCIWatch(args []string) (*ciWatchInvocation, bool) {
	call := &ciWatchInvocation{seconds: client.CIWatchDefaultSeconds}
	waitSet := false
	for i := 0; i < len(args); i++ {
		flag, raw, assigned := strings.Cut(args[i], "=")
		if flag != "--target" && flag != "--wait-seconds" {
			return nil, false
		}
		if !assigned {
			i++
			if i >= len(args) {
				return nil, false
			}
			raw = args[i]
		}
		if flag == "--wait-seconds" {
			n, err := strconv.Atoi(raw)
			if waitSet || err != nil || strconv.Itoa(n) != raw || client.ValidateReviewOperationWait(n) != nil {
				return nil, false
			}
			call.seconds, waitSet = n, true
			continue
		}
		parts := strings.Split(raw, ":")
		if len(parts) != 2 && len(parts) != 3 {
			return nil, false
		}
		number, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || strconv.FormatInt(number, 10) != parts[0] || len(parts) == 3 && parts[2] == "" {
			return nil, false
		}
		target := client.CIWatchTarget{PullNumber: number, HeadSHA: parts[1]}
		if len(parts) == 3 {
			target.BaseSHA = parts[2]
		}
		call.targets = append(call.targets, target)
	}
	return call, client.ValidateCIWatchTargets(call.targets) == nil
}
