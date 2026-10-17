package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
)

type mapImportInvocation struct {
	operation, file, preview, key, source string
	revision                              int64
	page                                  client.PageOptions
	document                              json.RawMessage
}

func parseMapImport(operation string, args []string) (*mapImportInvocation, bool) {
	m := &mapImportInvocation{operation: operation, page: client.PageOptions{Limit: 25}}
	allowed := map[string]bool{}
	positions := 0
	switch operation {
	case "import-preview":
		allowed["--file"] = true
	case "import-apply":
		allowed["--file"], allowed["--preview-sha256"], allowed["--key"] = true, true, true
	case "import-read":
		positions = 1
	case "import-history":
		positions = 1
		allowed["--limit"], allowed["--cursor"] = true, true
	case "import-revision":
		positions = 2
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
	if positions > 0 {
		m.source = positional[0]
		if !client.ValidMapImportSourceKey(m.source) {
			return nil, false
		}
	}
	if positions == 2 {
		n, e := strconv.ParseInt(positional[1], 10, 64)
		if e != nil || n < 1 || strconv.FormatInt(n, 10) != positional[1] {
			return nil, false
		}
		m.revision = n
	}
	if v, ok := values["--limit"]; ok {
		n, e := strconv.Atoi(v)
		if e != nil || n < 1 || n > 100 {
			return nil, false
		}
		m.page.Limit = n
	}
	if v, ok := values["--cursor"]; ok {
		if !client.ValidMapImportCursor(v) {
			return nil, false
		}
		m.page.Cursor = v
	}
	if positions == 0 {
		m.file = values["--file"]
		if !filepath.IsAbs(m.file) || len(m.file) > 4096 {
			return nil, false
		}
	}
	if operation == "import-apply" {
		m.key, m.preview = values["--key"], values["--preview-sha256"]
		if !client.ValidMutationKey(m.key) || len(m.preview) != 64 || strings.Trim(m.preview, "0123456789abcdef") != "" {
			return nil, false
		}
	}
	return m, true
}

func prepareMapImport(ctx context.Context, m *mapImportInvocation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.file == "" {
		return nil
	}
	invalid := func() error {
		return &client.Error{Code: "invalid_import_file", Message: "Select an owner-private regular map import JSON file within 256 KiB.", Action: "Correct the selected file locally and retry the same intended request.", Status: 400}
	}
	private := func(i os.FileInfo) bool {
		return i != nil && i.Mode().IsRegular() && i.Size() <= 256<<10 && (runtime.GOOS == "windows" || i.Mode().Perm()&0077 == 0)
	}
	before, err := os.Lstat(m.file)
	if err != nil || !private(before) {
		return invalid()
	}
	f, err := os.Open(m.file)
	if err != nil {
		return invalid()
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !private(after) || !os.SameFile(before, after) {
		return invalid()
	}
	pathAfter, err := os.Lstat(m.file)
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
	if err = client.ValidateMapImportDocument(data); err != nil {
		return err
	}
	if m.operation == "import-apply" {
		if err = client.ValidateMapImportApply(m.key, m.preview, data); err != nil {
			return err
		}
	}
	m.document = data
	return nil
}

func executeMapImport(ctx, parent context.Context, c *client.Client, m *mapImportInvocation, stdout, stderr io.Writer) int {
	var value any
	var err error
	switch m.operation {
	case "import-preview":
		value, err = c.PreviewMapImport(ctx, m.document)
	case "import-apply":
		value, err = c.ApplyMapImport(ctx, m.key, m.preview, m.document)
	case "import-read":
		value, err = c.MapImport(ctx, m.source)
	case "import-history":
		value, err = c.MapImportHistory(ctx, m.source, m.page)
	case "import-revision":
		value, err = c.MapImportRevision(ctx, m.source, m.revision)
	}
	if err != nil {
		return reportClientError(stderr, err, parent)
	}
	if m.operation != "import-apply" {
		return emit(stdout, stderr, value)
	}
	data, err := json.Marshal(value)
	if err == nil {
		data = append(data, '\n')
		var n int
		n, err = stdout.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The map import receipt could not be written.", Action: "Retry the exact same file contents, preview fingerprint, key, worker and bound instance; do not invent a replacement key.", OutcomeUnknown: true})
	}
	return ExitSuccess
}
