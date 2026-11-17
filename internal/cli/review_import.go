package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
)

type reviewImportInvocation struct {
	apply              bool
	file, preview, key string
	document           json.RawMessage
}

func parseReviewImport(call invocation, args []string) (invocation, bool) {
	in := &reviewImportInvocation{apply: call.subcommand == "import-apply"}
	values := map[string]string{}
	for i := 0; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
		if flag != "--file" && (!in.apply || flag != "--preview" && flag != "--key") {
			return call, false
		}
		if _, exists := values[flag]; exists {
			return call, false
		}
		if !assigned {
			i++
			if i >= len(args) {
				return call, false
			}
			value = args[i]
		}
		if value == "" || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return call, false
		}
		values[flag] = value
	}
	in.file, in.preview, in.key = values["--file"], values["--preview"], values["--key"]
	if !filepath.IsAbs(in.file) || len(in.file) > 4096 {
		return call, false
	}
	if in.apply && (!client.ValidMutationKey(in.key) || len(in.preview) != 64 || strings.IndexFunc(in.preview, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }) >= 0) {
		return call, false
	}
	call.reviewImport = in
	return call, true
}
func prepareReviewImport(ctx context.Context, in *reviewImportInvocation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	invalid := func() error {
		return &client.Error{Code: "invalid_import_file", Message: "The selected import must be a readable regular JSON file within 256 KiB.", Action: "Check the selected file locally, correct it and retry.", Status: 400}
	}
	before, err := os.Lstat(in.file)
	if err != nil || !before.Mode().IsRegular() || before.Size() > 256<<10 {
		return invalid()
	}
	f, err := os.Open(in.file)
	if err != nil {
		return invalid()
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !actual.Mode().IsRegular() || !os.SameFile(before, actual) || actual.Size() > 256<<10 {
		return invalid()
	}
	b, err := io.ReadAll(io.LimitReader(f, (256<<10)+1))
	if err != nil || len(b) > 256<<10 {
		return invalid()
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = client.ValidateReviewImportDocument(b); err != nil {
		return err
	}
	if in.apply {
		if err = client.ValidateReviewImportApply(in.key, in.preview, b); err != nil {
			return err
		}
	}
	in.document = b
	return nil
}
func executeReviewImport(ctx, parent context.Context, c *client.Client, in *reviewImportInvocation, stdout, stderr io.Writer) int {
	if !in.apply {
		v, err := c.PreviewReviewImport(ctx, in.document)
		if err != nil {
			return reportClientError(stderr, err, parent)
		}
		return emit(stdout, stderr, v)
	}
	v, err := c.ApplyReviewImport(ctx, in.key, in.preview, in.document)
	if err != nil {
		return reportClientError(stderr, err, parent)
	}
	if err = json.NewEncoder(stdout).Encode(v); err != nil {
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The import receipt could not be written.", Action: "Retry with the exact same file contents, preview fingerprint, key and bound instance; do not invent a replacement key.", OutcomeUnknown: true})
	}
	return ExitSuccess
}
