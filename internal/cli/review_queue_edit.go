package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
)

type reviewQueueEditInvocation struct {
	file, key string
	document  json.RawMessage
}

func parseReviewQueueEdit(call invocation, args []string) (invocation, bool) {
	values := map[string]string{}
	for i := 0; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
		if flag != "--file" && flag != "--key" {
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
	file, key := values["--file"], values["--key"]
	if !filepath.IsAbs(file) || len(file) > 4096 || !client.ValidMutationKey(key) {
		return call, false
	}
	call.reviewQueueEdit = &reviewQueueEditInvocation{file: file, key: key}
	return call, true
}

func prepareReviewQueueEdit(ctx context.Context, in *reviewQueueEditInvocation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	invalid := func() error {
		return &client.Error{Code: "invalid_queue_edit_file", Message: "Select an owner-private regular queue edit JSON file within 256 KiB.", Action: "Correct the selected file locally and retain its exact contents and idempotency key.", Status: 400}
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
	actual, err := f.Stat()
	if err != nil || !private(actual) || !os.SameFile(before, actual) {
		return invalid()
	}
	pathAfter, err := os.Lstat(in.file)
	if err != nil || !private(pathAfter) || !os.SameFile(before, pathAfter) {
		return invalid()
	}
	document, err := io.ReadAll(io.LimitReader(f, (256<<10)+1))
	if err != nil || len(document) > 256<<10 {
		return invalid()
	}
	actual, err = f.Stat()
	if err != nil || !private(actual) || !os.SameFile(before, actual) {
		return invalid()
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = client.ValidateReviewQueueEdit(in.key, document); err != nil {
		return err
	}
	in.document = document
	return nil
}

func executeReviewQueueEdit(ctx, parent context.Context, c *client.Client, in *reviewQueueEditInvocation, stdout, stderr io.Writer) int {
	receipt, err := c.EditReviewQueue(ctx, in.key, in.document)
	if err != nil {
		return reportClientError(stderr, err, parent)
	}
	body, err := json.Marshal(receipt)
	if err == nil {
		body = append(body, '\n')
		var n int
		n, err = stdout.Write(body)
		if err == nil && n != len(body) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The queue edit receipt could not be written.", Action: "Retry with the exact same file contents, idempotency key and bound instance; do not invent a replacement key.", OutcomeUnknown: true})
	}
	return ExitSuccess
}
