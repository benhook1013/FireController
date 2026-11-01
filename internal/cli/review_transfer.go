package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/benhook1013/FireController/internal/client"
)

type reviewTransferInvocation struct {
	apply              bool
	file, preview, key string
	document           json.RawMessage
}

func parseReviewTransfer(call invocation, args []string) (invocation, bool) {
	// The transfer commands deliberately share the prepared import flag grammar.
	syntax := call
	syntax.subcommand = "import-preview"
	if call.subcommand == "transfer-apply" {
		syntax.subcommand = "import-apply"
	}
	parsed, ok := parseReviewImport(syntax, args)
	if !ok {
		return call, false
	}
	in := parsed.reviewImport
	call.reviewTransfer = &reviewTransferInvocation{apply: in.apply, file: in.file, preview: in.preview, key: in.key}
	return call, true
}
func prepareReviewTransfer(ctx context.Context, in *reviewTransferInvocation) error {
	invalid := func() error {
		return &client.Error{Code: "invalid_transfer_file", Message: "Select an owner-private regular review transfer JSON file within 256 KiB.", Action: "Correct the selected file locally and retry the same intended request.", Status: 400}
	}
	data, err := readOwnerPrivateJSON(ctx, in.file)
	if errors.Is(err, errInvalidOwnerPrivateJSONFile) {
		return invalid()
	}
	if err != nil {
		return err
	}
	if err = client.ValidateReviewTransferDocument(data); err != nil {
		return err
	}
	if in.apply {
		if err = client.ValidateReviewTransferApply(in.key, in.preview, data); err != nil {
			return err
		}
	}
	in.document = data
	return nil
}
func executeReviewTransfer(ctx, parent context.Context, c *client.Client, in *reviewTransferInvocation, stdout, stderr io.Writer) int {
	if !in.apply {
		value, err := c.PreviewReviewTransfer(ctx, in.document)
		if err != nil {
			return reportClientError(stderr, err, parent)
		}
		return emit(stdout, stderr, value)
	}
	value, err := c.ApplyReviewTransfer(ctx, in.key, in.preview, in.document)
	if err != nil {
		return reportClientError(stderr, err, parent)
	}
	encoded, err := json.Marshal(value)
	if err == nil {
		encoded = append(encoded, '\n')
		var n int
		n, err = stdout.Write(encoded)
		if err == nil && n != len(encoded) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The transfer receipt could not be written.", Action: "Retry with the exact same file contents, preview fingerprint, key and bound instance; do not invent a replacement key.", OutcomeUnknown: true})
	}
	return ExitSuccess
}
