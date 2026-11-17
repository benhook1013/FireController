package cli

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
)

type publicationInvocation struct {
	operation, file, key, preview string
	output                        string
	revision                      int64
	input                         client.PublicationInput
	changes                       client.PublicationChanges
}

func parsePublication(operation string, args []string) (*publicationInvocation, bool) {
	in := &publicationInvocation{operation: operation}
	allowed := map[string]bool{}
	switch operation {
	case "read":
	case "preview":
		allowed["--file"] = true
	case "prepare":
		allowed["--file"], allowed["--output"] = true, true
	case "apply":
		allowed["--file"], allowed["--preview-sha256"], allowed["--key"] = true, true, true
	case "unpublish":
		allowed["--expect-revision"], allowed["--key"] = true, true
	default:
		return nil, false
	}
	values := map[string]string{}
	for i := 0; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
		if !allowed[flag] {
			return nil, false
		}
		if _, duplicate := values[flag]; duplicate {
			return nil, false
		}
		if !assigned {
			i++
			if i >= len(args) {
				return nil, false
			}
			value = args[i]
		}
		if value == "" || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return nil, false
		}
		values[flag] = value
	}
	if operation == "preview" || operation == "apply" || operation == "prepare" {
		in.file = values["--file"]
		if !filepath.IsAbs(in.file) || len(in.file) > 4096 {
			return nil, false
		}
	}
	if operation == "prepare" {
		in.output = values["--output"]
		if !filepath.IsAbs(in.output) || len(in.output) > 4096 || filepath.Clean(in.output) == filepath.Clean(in.file) {
			return nil, false
		}
	}
	if operation == "apply" {
		in.key, in.preview = values["--key"], values["--preview-sha256"]
		if !validMutationKey(in.key) || len(in.preview) != 64 || strings.IndexFunc(in.preview, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }) >= 0 {
			return nil, false
		}
	}
	if operation == "unpublish" {
		in.key = values["--key"]
		revision, err := strconv.ParseInt(values["--expect-revision"], 10, 64)
		if err != nil || revision < 0 || revision == math.MaxInt64 || client.ValidatePublicationUnpublish(in.key, revision) != nil {
			return nil, false
		}
		in.revision = revision
	}
	return in, true
}

func preparePublication(ctx context.Context, in *publicationInvocation) error {
	if in.operation != "preview" && in.operation != "apply" && in.operation != "prepare" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	invalid := func() error {
		return &client.Error{Code: "invalid_publication_file", Message: "Select a bounded owner-private regular approved publication JSON file.", Action: "Correct the file locally; retain its exact contents, preview hash and key for apply recovery.", Status: 400}
	}
	private := func(info os.FileInfo) bool {
		return info.Mode().IsRegular() && info.Size() <= client.MaxPublicationDocumentBytes && (runtime.GOOS == "windows" || info.Mode().Perm()&0077 == 0)
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
	actual, err := f.Stat()
	if err != nil || !private(actual) || !os.SameFile(before, actual) {
		return invalid()
	}
	raw, err := io.ReadAll(io.LimitReader(f, client.MaxPublicationDocumentBytes+1))
	if err != nil || len(raw) > client.MaxPublicationDocumentBytes {
		return invalid()
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if in.operation == "prepare" {
		in.changes, err = client.DecodePublicationChanges(raw)
		if err != nil {
			return err
		}
		return preparePublicationOutput(in)
	}
	in.input, err = client.DecodePublicationInput(raw)
	if err != nil {
		return invalid()
	}
	if in.operation == "apply" && client.ValidatePublicationApply(in.key, in.preview, in.input) != nil {
		return invalid()
	}
	return nil
}

func executePublication(ctx, parent context.Context, c *client.Client, in *publicationInvocation, stdout, stderr io.Writer) int {
	var value any
	var err error
	switch in.operation {
	case "prepare":
		value, err = executePublicationPreparation(ctx, c, in)
	case "read":
		value, err = c.Publication(ctx)
	case "preview":
		value, err = c.PreviewPublication(ctx, in.input)
	case "apply":
		value, err = c.ApplyPublication(ctx, in.key, in.preview, in.input)
	case "unpublish":
		value, err = c.Unpublish(ctx, in.key, in.revision)
	}
	if err != nil {
		return reportClientError(stderr, err, parent)
	}
	if in.operation == "prepare" {
		return emitPreparedPublication(stdout, stderr, value)
	}
	if in.operation != "apply" && in.operation != "unpublish" {
		return emit(stdout, stderr, value)
	}
	raw, err := json.Marshal(value)
	if err == nil {
		raw = append(raw, '\n')
		var written int
		written, err = stdout.Write(raw)
		if err == nil && written != len(raw) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The publication receipt could not be written.", Action: "Retry the exact same key, file contents, preview hash or unpublish revision and selected binding.", OutcomeUnknown: true})
	}
	return ExitSuccess
}
