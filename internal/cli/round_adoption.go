package cli

import (
	"context"
	"encoding/json"
	"io"

	"github.com/benhook1013/FireController/internal/client"
)

type roundAdoptionInvocation reviewImportInvocation

func parseRoundAdoption(call invocation, args []string) (invocation, bool) {
	if len(args) < 2 || args[0] != "preview" && args[0] != "apply" {
		return call, false
	}
	syntax := call
	syntax.subcommand = "import-" + args[0]
	parsed, ok := parseReviewImport(syntax, args[1:])
	if !ok {
		return call, false
	}
	call.roundAdoption = (*roundAdoptionInvocation)(parsed.reviewImport)
	return call, true
}

func prepareRoundAdoption(ctx context.Context, in *roundAdoptionInvocation) error {
	raw, err := readPrivateImportFile(ctx, in.file, 16<<10)
	if err != nil {
		return err
	}
	if err = client.ValidateReviewRoundAdoptionDocument(raw); err != nil {
		return err
	}
	if in.apply {
		if err = client.ValidateReviewRoundAdoptionApply(in.key, in.preview, raw); err != nil {
			return err
		}
	}
	in.document = raw
	return nil
}

func executeRoundAdoption(ctx, parent context.Context, c *client.Client, in *roundAdoptionInvocation, stdout, stderr io.Writer) int {
	if !in.apply {
		value, err := c.PreviewReviewRoundAdoption(ctx, in.document)
		if err != nil {
			return reportClientError(stderr, err, parent)
		}
		return emit(stdout, stderr, value)
	}
	value, err := c.ApplyReviewRoundAdoption(ctx, in.key, in.preview, in.document)
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
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The adoption receipt could not be written.", Action: "Retry the exact same file, preview fingerprint, key and bound instance; do not invent a replacement key.", OutcomeUnknown: true})
	}
	return ExitSuccess
}
