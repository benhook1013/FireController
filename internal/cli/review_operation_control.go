package cli

import (
	"github.com/benhook1013/FireController/internal/client"
	"strconv"
)

type reviewOperationControlInvocation struct {
	id, key string
	control client.ReviewOperationControl
}

func parseReviewOperationControl(args []string) (*reviewOperationControlInvocation, bool) {
	if len(args) < 1 {
		return nil, false
	}
	in := &reviewOperationControlInvocation{id: args[0]}
	values, ok := reviewSourceFlags(args[1:], "--action", "--expect-revision", "--key")
	if !ok {
		return nil, false
	}
	in.key = values["--key"]
	in.control.Action = values["--action"]
	n, err := strconv.ParseInt(values["--expect-revision"], 10, 64)
	in.control.ExpectedRevision = n
	return in, err == nil && strconv.FormatInt(n, 10) == values["--expect-revision"] && validMutationKey(in.key) && client.ValidateReviewOperationControl(in.id, in.control) == nil
}
