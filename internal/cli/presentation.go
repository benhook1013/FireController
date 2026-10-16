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

type presentationInvocation struct {
	operation, file, key string
	revision             int64
	config               client.PresentationConfig
	page                 client.PageOptions
}

func parsePresentation(operation string, args []string) (*presentationInvocation, bool) {
	in := &presentationInvocation{operation: operation, page: client.PageOptions{Limit: 25}}
	allowed := map[string]bool{}
	positions := 0
	switch operation {
	case "read":
	case "history":
		allowed["--limit"], allowed["--cursor"] = true, true
	case "revision":
		positions = 1
	case "replace":
		allowed["--file"], allowed["--key"], allowed["--expect-revision"] = true, true, true
	default:
		return nil, false
	}
	values := map[string]string{}
	var positional []string
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			positional = append(positional, args[i])
			continue
		}
		flag, value, assigned := strings.Cut(args[i], "=")
		if !allowed[flag] {
			return nil, false
		}
		if _, exists := values[flag]; exists {
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
	if len(positional) != positions {
		return nil, false
	}
	if positions == 1 {
		n, err := strconv.ParseInt(positional[0], 10, 64)
		if err != nil || n < 1 {
			return nil, false
		}
		in.revision = n
	}
	if v, ok := values["--limit"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return nil, false
		}
		in.page.Limit = n
	}
	if v, ok := values["--cursor"]; ok {
		if len(v) > 4096 || v != strings.TrimSpace(v) {
			return nil, false
		}
		in.page.Cursor = v
	}
	if operation == "replace" {
		in.file, in.key = values["--file"], values["--key"]
		n, err := strconv.ParseInt(values["--expect-revision"], 10, 64)
		if err != nil || n < 0 || n == math.MaxInt64 || !validMutationKey(in.key) || !filepath.IsAbs(in.file) || len(in.file) > 4096 {
			return nil, false
		}
		in.revision = n
	}
	return in, true
}

func preparePresentation(ctx context.Context, in *presentationInvocation) error {
	if in.operation != "replace" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	invalid := func() error {
		return &client.Error{Code: "invalid_presentation_file", Message: "Select a readable owner-private regular presentation JSON file within 256 KiB, with at most 16 KiB of canonical settings.", Action: "Correct the selected file locally and retain its contents and key for the intended request.", Status: 400}
	}
	before, err := os.Lstat(in.file)
	if err != nil || !before.Mode().IsRegular() || runtime.GOOS != "windows" && before.Mode().Perm()&0o077 != 0 || before.Size() > 256<<10 {
		return invalid()
	}
	f, err := os.Open(in.file)
	if err != nil {
		return invalid()
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !actual.Mode().IsRegular() || runtime.GOOS != "windows" && actual.Mode().Perm()&0o077 != 0 || !os.SameFile(before, actual) || actual.Size() > 256<<10 {
		return invalid()
	}
	raw, err := io.ReadAll(io.LimitReader(f, (256<<10)+1))
	if err != nil || len(raw) > 256<<10 {
		return invalid()
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	in.config, err = client.DecodePresentationConfig(raw)
	if err != nil {
		return invalid()
	}
	return nil
}

func executePresentation(ctx, parent context.Context, c *client.Client, in *presentationInvocation, stdout, stderr io.Writer) int {
	var value any
	var err error
	switch in.operation {
	case "read":
		value, err = c.Presentation(ctx)
	case "history":
		value, err = c.PresentationHistory(ctx, in.page)
	case "revision":
		value, err = c.PresentationRevision(ctx, in.revision)
	case "replace":
		value, err = c.ReplacePresentation(ctx, in.key, client.ReplacePresentationRequest{ExpectedRevision: in.revision, Presentation: in.config})
	}
	if err != nil {
		return reportClientError(stderr, err, parent)
	}
	if in.operation != "replace" {
		return emit(stdout, stderr, value)
	}
	raw, err := json.Marshal(value)
	if err == nil {
		raw = append(raw, '\n')
		var n int
		n, err = stdout.Write(raw)
		if err == nil && n != len(raw) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The presentation receipt could not be written.", Action: "Retry the exact same key, file contents, expected revision and bound instance; do not invent a replacement key.", OutcomeUnknown: true})
	}
	return ExitSuccess
}
