package cli

import (
	"context"
	"encoding/json"
	"io"

	"github.com/benhook1013/FireController/internal/client"
)

type coordinationImportInvocation struct {
	apply, state       bool
	file, preview, key string
	document           json.RawMessage
}

func parseCoordinationImport(call invocation, args []string) (invocation, bool) {
	if call.subcommand == "state" {
		call.coordinationImport = &coordinationImportInvocation{state: true}
		return call, len(args) == 0
	}
	if call.subcommand != "preview" && call.subcommand != "apply" {
		return call, false
	}
	// The coordination import commands share the prepared import flag grammar.
	syntax := call
	syntax.subcommand = "import-preview"
	if call.subcommand == "apply" {
		syntax.subcommand = "import-apply"
	}
	parsed, ok := parseReviewImport(syntax, args)
	if !ok {
		return call, false
	}
	in := parsed.reviewImport
	call.coordinationImport = &coordinationImportInvocation{apply: in.apply, file: in.file, preview: in.preview, key: in.key}
	return call, true
}
func prepareCoordinationImport(ctx context.Context, in *coordinationImportInvocation) error {
	if in.state {
		return nil
	}
	data, err := readPrivateImportFile(ctx, in.file, 256<<10)
	if err != nil {
		return err
	}
	if err = client.ValidateCoordinationImportDocument(data); err != nil {
		return err
	}
	if in.apply {
		if err = client.ValidateCoordinationImportApply(in.key, in.preview, data); err != nil {
			return err
		}
	}
	in.document = data
	return nil
}
func executeCoordinationImport(ctx, parent context.Context, c *client.Client, in *coordinationImportInvocation, stdout, stderr io.Writer) int {
	if in.state {
		value, err := c.CoordinationImportState(ctx)
		if err != nil {
			return reportClientError(stderr, err, parent)
		}
		return emit(stdout, stderr, value)
	}
	if !in.apply {
		value, err := c.PreviewCoordinationImport(ctx, in.document)
		if err != nil {
			return reportClientError(stderr, err, parent)
		}
		return emit(stdout, stderr, value)
	}
	value, err := c.ApplyCoordinationImport(ctx, in.key, in.preview, in.document)
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
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The import receipt could not be written.", Action: "Retry with the exact same file contents, preview fingerprint, key and bound instance; do not invent a replacement key.", OutcomeUnknown: true})
	}
	return ExitSuccess
}
