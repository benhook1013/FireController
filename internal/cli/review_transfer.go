package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"runtime"

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
	if err := ctx.Err(); err != nil {
		return err
	}
	invalid := func() error {
		return &client.Error{Code: "invalid_transfer_file", Message: "Select an owner-private regular review transfer JSON file within 256 KiB.", Action: "Correct the selected file locally and retry the same intended request.", Status: 400}
	}
	private := func(info os.FileInfo) bool {
		return info != nil && info.Mode().IsRegular() && info.Size() <= 256<<10 && (runtime.GOOS == "windows" || info.Mode().Perm()&0077 == 0)
	}
	before, err := os.Lstat(in.file)
	if err != nil || !private(before) {
		return invalid()
	}
	f, err := os.Open(in.file)
	if err != nil {
		return invalid()
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !private(after) || !os.SameFile(before, after) {
		return invalid()
	}
	pathAfter, err := os.Lstat(in.file)
	if err != nil || !private(pathAfter) || !os.SameFile(before, pathAfter) {
		return invalid()
	}
	data, err := io.ReadAll(io.LimitReader(f, (256<<10)+1))
	if err != nil || len(data) > 256<<10 {
		return invalid()
	}
	after, err = f.Stat()
	if err != nil || !private(after) || !os.SameFile(before, after) {
		return invalid()
	}
	if err = ctx.Err(); err != nil {
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
