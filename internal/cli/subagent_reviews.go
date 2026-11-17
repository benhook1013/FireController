package cli

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"

	"github.com/benhook1013/FireController/internal/client"
)

type subagentReviewInvocation struct {
	operation string
	id        string
	file      string
	key       string
	record    client.SubagentRecordRequest
	update    client.SubagentUpdateRequest
}

func parseSubagentReview(call invocation, args []string) (invocation, bool) {
	in := &subagentReviewInvocation{operation: call.subcommand}
	if in.operation == "subagent-read" {
		if len(args) != 1 || client.ValidateSubagentRunID(args[0]) != nil {
			return call, false
		}
		in.id = args[0]
		call.subagentReview = in
		return call, true
	}
	if in.operation == "subagent-update" {
		if len(args) == 0 || client.ValidateSubagentRunID(args[0]) != nil {
			return call, false
		}
		in.id = args[0]
		args = args[1:]
	}
	values, ok := reviewRunFlags(args, []string{"--file", "--key"})
	if !ok || !filepath.IsAbs(values["--file"]) || len(values["--file"]) > 4096 || !client.ValidMutationKey(values["--key"]) {
		return call, false
	}
	in.file, in.key = values["--file"], values["--key"]
	call.subagentReview = in
	return call, true
}
func prepareSubagentReview(ctx context.Context, in *subagentReviewInvocation) error {
	if in.operation == "subagent-read" {
		return nil
	}
	body, err := readOwnerPrivateJSON(ctx, in.file)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &client.Error{Code: "invalid_subagent_file", Message: "Select an owner-private regular UTF-8 JSON file within256KiB.", Action: "Correct the selected file locally and retain the exact request and key for recovery.", Status: 400}
	}
	if in.operation == "subagent-record" {
		in.record, err = client.DecodeSubagentRecordDocument(body)
	} else {
		in.update, err = client.DecodeSubagentUpdateDocument(body)
	}
	return err
}
func executeSubagentReview(ctx, parent context.Context, c *client.Client, in *subagentReviewInvocation, stdout, stderr io.Writer) int {
	if in.operation == "subagent-read" {
		value, err := c.ReadSubagentReview(ctx, in.id)
		if err != nil {
			return reportClientError(stderr, err, parent)
		}
		return emit(stdout, stderr, value)
	}
	var value client.SubagentReceipt
	var err error
	if in.operation == "subagent-record" {
		value, err = c.RecordSubagentReview(ctx, in.record, in.key)
	} else {
		value, err = c.UpdateSubagentReview(ctx, in.id, in.update, in.key)
	}
	if err != nil {
		return reportClientError(stderr, err, parent)
	}
	body, err := json.Marshal(value)
	if err == nil {
		body = append(body, '\n')
		var n int
		n, err = stdout.Write(body)
		if err == nil && n != len(body) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The subagent review receipt could not be written.", Action: "Retry the same key, exact request and bound instance; never invent a replacement key.", OutcomeUnknown: true})
	}
	return ExitSuccess
}
