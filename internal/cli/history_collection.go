package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

type historyInvocation struct {
	operation, id, pageID, key, output, cursor string
	pulls                                      []int64
	revision                                   int64
	limit, waitSeconds                         int
}

func historyID(value, prefix string) bool {
	return strings.HasPrefix(value, prefix) && len(value) == len(prefix)+32 && strings.Trim(value[len(prefix):], "0123456789abcdef") == ""
}

func parseHistory(operation string, args []string) (*historyInvocation, bool) {
	h := &historyInvocation{operation: operation, limit: 5}
	allowed := map[string]bool{}
	positions := 1
	switch operation {
	case "collect":
		positions, h.waitSeconds = 0, 2
		allowed["--pulls"], allowed["--key"], allowed["--wait"] = true, true, true
	case "collection":
		allowed["--wait"] = true
	case "pages":
		allowed["--limit"], allowed["--cursor"] = true, true
	case "export-page":
		positions = 2
		allowed["--output"] = true
	case "resume", "cancel":
		allowed["--expect-revision"], allowed["--key"] = true, true
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
		if _, seen := values[flag]; seen {
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
		h.id = positional[0]
		if !historyID(h.id, "history_collection_") {
			return nil, false
		}
	}
	if positions == 2 {
		h.pageID = positional[1]
		if !historyID(h.pageID, "history_page_") {
			return nil, false
		}
	}
	if v, present := values["--wait"]; present {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 900 || strconv.Itoa(n) != v {
			return nil, false
		}
		h.waitSeconds = n
	}
	if v, present := values["--limit"]; present {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 || strconv.Itoa(n) != v {
			return nil, false
		}
		h.limit = n
	}
	if v, present := values["--cursor"]; present {
		if len(v) > 4096 || strings.ContainsAny(v, " \t\r\n") {
			return nil, false
		}
		h.cursor = v
	}
	if operation == "collect" {
		seen := map[int64]bool{}
		for _, v := range strings.Split(values["--pulls"], ",") {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 1 || n > 2147483647 || strconv.FormatInt(n, 10) != v || seen[n] || len(h.pulls) >= 100 {
				return nil, false
			}
			seen[n] = true
			h.pulls = append(h.pulls, n)
		}
	}
	if operation == "collect" || operation == "resume" || operation == "cancel" {
		h.key = values["--key"]
		if !client.ValidMutationKey(h.key) {
			return nil, false
		}
	}
	if operation == "resume" || operation == "cancel" {
		v := values["--expect-revision"]
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 || strconv.FormatInt(n, 10) != v {
			return nil, false
		}
		h.revision = n
	}
	if operation == "export-page" {
		h.output = values["--output"]
		if !filepath.IsAbs(h.output) || len(h.output) > 4096 || filepath.Base(h.output) == "." || filepath.Base(h.output) == string(filepath.Separator) {
			return nil, false
		}
	}
	return h, true
}

func executeHistory(ctx, parent context.Context, c *client.Client, h *historyInvocation, stdout, stderr io.Writer) int {
	var value any
	var err error
	switch h.operation {
	case "collect":
		value, err = c.CollectHistory(ctx, h.key, client.HistorySelection{PullNumbers: h.pulls}, h.waitSeconds)
	case "collection":
		value, err = c.HistoryCollection(ctx, h.id, h.waitSeconds)
	case "pages":
		value, err = c.HistoryPages(ctx, h.id, h.limit, h.cursor)
	case "resume", "cancel":
		value, err = c.ChangeHistoryCollection(ctx, h.id, h.operation, h.key, h.revision)
	case "export-page":
		// Only an explicitly selected immutable capture may expand provider text.
		var capture client.HistoryCapture
		capture, err = c.HistoryCapture(ctx, h.id, h.pageID)
		if err == nil {
			value, err = exportHistoryPage(ctx, h, capture)
		}
	}
	if err != nil {
		return reportClientError(stderr, err, parent)
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
		mutation := h.operation == "collect" || h.operation == "resume" || h.operation == "cancel"
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The collection result could not be written.", Action: "For a mutation retry the exact same key, request and binding; for an export inspect the selected output file before retrying.", OutcomeUnknown: mutation})
	}
	return ExitSuccess
}

type historyExportReceipt struct {
	CollectionID string `json:"collection_id"`
	PageID       string `json:"page_id"`
	Path         string `json:"path"`
	Bytes        int    `json:"bytes"`
	SHA256       string `json:"sha256"`
}

func prepareHistoryExport(path string) error {
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return &client.Error{Code: "export_exists", Message: "The export path exists or is unavailable.", Action: "Choose a new absolute output path; existing files and symlinks are never replaced.", Status: 400}
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil || !parent.IsDir() {
		return &client.Error{Code: "export_failed", Message: "The export directory is unavailable.", Action: "Choose an existing output directory.", Status: 400}
	}
	return nil
}

// Publish the completed private file atomically without replacing an existing
// path, including a symlink. No provider text is written to the agent's stdout.
func exportHistoryPage(ctx context.Context, h *historyInvocation, capture client.HistoryCapture) (historyExportReceipt, error) {
	var receipt historyExportReceipt
	failure := func(code string) error {
		return &client.Error{Code: code, Message: "The private capture could not be exported to the selected path.", Action: "Choose a new absolute output path in an existing directory; existing files are never replaced.", Status: 400}
	}
	data, err := json.Marshal(capture)
	if err != nil {
		return receipt, failure("export_failed")
	}
	data = append(data, '\n')
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	file, err := os.CreateTemp(filepath.Dir(h.output), ".fc-history-*")
	if err != nil {
		return receipt, failure("export_failed")
	}
	defer file.Close()
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return receipt, failure("export_failed")
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if err := os.Link(file.Name(), h.output); err != nil {
		if errors.Is(err, os.ErrExist) {
			return receipt, failure("export_exists")
		}
		return receipt, failure("export_failed")
	}
	// The final path now exists. A later durability failure must preserve it.
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(h.output))
		if err == nil {
			err = dir.Sync()
			_ = dir.Close()
		}
		if err != nil {
			return receipt, &client.Error{Code: "export_uncertain", Message: "The export file was published but directory durability could not be confirmed.", Action: "Inspect the selected output file before choosing a new path."}
		}
	}
	hash := sha256.Sum256(data)
	receipt = historyExportReceipt{CollectionID: h.id, PageID: h.pageID, Path: h.output, Bytes: len(data), SHA256: hex.EncodeToString(hash[:])}
	return receipt, nil
}
