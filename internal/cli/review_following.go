package cli

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/client"
)

type reviewFollowingInvocation struct {
	number int64
	key    string
	read   bool
	input  client.ConfigureReviewFollowingRequest
}

func parseReviewFollowing(call invocation, args []string) (invocation, bool) {
	if len(args) < 1 {
		return call, false
	}
	number, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || number < 1 || number > 2147483647 || strconv.FormatInt(number, 10) != args[0] {
		return call, false
	}
	in := &reviewFollowingInvocation{number: number, read: call.subcommand == "following", input: client.ConfigureReviewFollowingRequest{Following: call.subcommand == "follow"}}
	seen := map[string]bool{}
	for i := 1; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
		if in.read || seen[flag] || flag != "--expect-revision" && flag != "--key" {
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
		if flag == "--key" {
			in.key = value
		} else {
			n, e := strconv.ParseInt(value, 10, 64)
			if e != nil || n < 0 || n == int64(^uint64(0)>>1) || strconv.FormatInt(n, 10) != value {
				return call, false
			}
			in.input.ExpectedRevision = n
		}
	}
	if !in.read && (!seen["--expect-revision"] || !client.ValidMutationKey(in.key)) {
		return call, false
	}
	call.reviewFollowing = in
	return call, true
}
func executeReviewFollowing(ctx, parent context.Context, c *client.Client, in *reviewFollowingInvocation, stdout, stderr io.Writer) int {
	var out client.ReviewFollowing
	var err error
	if in.read {
		out, err = c.ReviewFollowing(ctx, in.number)
	} else {
		out, err = c.ConfigureReviewFollowing(ctx, in.key, in.number, in.input)
	}
	if err != nil {
		return reportClientError(stderr, err, parent)
	}
	body, _ := json.Marshal(out)
	body = append(body, '\n')
	n, err := stdout.Write(body)
	if err != nil || n != len(body) {
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The following result could not be written.", Action: "Read following again, or retry the exact mutation with its original key and binding.", OutcomeUnknown: !in.read})
	}
	return ExitSuccess
}
func reviewFollowingHelp(operation string) (operationHelpResponse, bool) {
	if operation != "reviews following" && operation != "reviews follow" && operation != "reviews unfollow" {
		return operationHelpResponse{}, false
	}
	out := operationHelpResponse{Usage: "<bound launcher> " + operation + " PR", Required: []string{"PR"}, Optional: []string{}, Documentation: "docs/review-notifications.md", Semantics: []string{"Controls only this bound worker's durable PR notifications. Ordinary explicit work follows automatically; an explicit unfollow survives later work.", "Following does not enroll or observe an untracked PR, assign ownership, request reviews or change CI/merge permissions. Notifications cover verified open-to-merged/closed transitions; existing closed history is not replayed."}, Example: "<bound launcher> " + operation + " 42"}
	if operation != "reviews following" {
		out.Usage += " --expect-revision N --key KEY"
		out.Required = append(out.Required, "--expect-revision N (zero for no preference)", "--key KEY")
		out.Example += " --expect-revision 0 --key pr-42-preference"
		out.Semantics = append(out.Semantics, "Read reviews following PR first. After an unknown result, retry the exact request, key and binding.")
	}
	return out, true
}
