package cli

import (
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
)

type projectMapInvocation struct {
	operation, id, key, file string
	revision                 int64
	entry                    client.ProjectMapEditable
	changes                  client.ProjectMapChanges
	association              client.ReplaceJobWorkstreamRequest
	filter                   client.ProjectMapFilter
	page                     client.PageOptions
}

func (m *projectMapInvocation) write() bool {
	return m.operation == "create" || m.operation == "replace" || m.operation == "update" || m.operation == "workstream-set"
}
func parseProjectMap(command, operation string, args []string) (*projectMapInvocation, bool) {
	m := &projectMapInvocation{operation: operation, page: client.PageOptions{Limit: 25}}
	allowed := map[string]bool{}
	positions := 0
	switch command + " " + operation {
	case "map create":
		allowed["--file"], allowed["--key"] = true, true
	case "map replace", "map update":
		positions = 1
		allowed["--file"], allowed["--key"], allowed["--expect-revision"] = true, true, true
	case "map list":
		allowed["--kind"], allowed["--archived"], allowed["--limit"], allowed["--cursor"] = true, true, true, true
	case "map read", "jobs workstream":
		positions = 1
	case "map history", "jobs workstream-history":
		positions = 1
		allowed["--limit"], allowed["--cursor"] = true, true
	case "map revision":
		positions = 2
	case "jobs workstream-set":
		positions = 1
		allowed["--workstream"], allowed["--clear"], allowed["--expect-revision"], allowed["--key"] = true, true, true, true
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
		if _, duplicate := values[flag]; duplicate {
			return nil, false
		}
		if flag == "--clear" {
			if assigned {
				return nil, false
			}
			values[flag] = "true"
			continue
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
		m.id = positional[0]
		if !validResourceID(m.id) {
			return nil, false
		}
	}
	if operation == "revision" {
		n, err := strconv.ParseInt(positional[1], 10, 64)
		if err != nil || n < 1 {
			return nil, false
		}
		m.revision = n
	}
	if v, ok := values["--limit"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return nil, false
		}
		m.page.Limit = n
	}
	if v, ok := values["--cursor"]; ok {
		if len(v) > 4096 || v != strings.TrimSpace(v) {
			return nil, false
		}
		m.page.Cursor = v
	}
	m.filter.Kind = values["--kind"]
	if m.filter.Kind != "" && m.filter.Kind != "workstream" && m.filter.Kind != "return_point" && m.filter.Kind != "section" {
		return nil, false
	}
	if v, ok := values["--archived"]; ok {
		if v != "true" && v != "false" {
			return nil, false
		}
		m.filter.Archived = v == "true"
	}
	if !m.write() {
		return m, true
	}
	m.key = values["--key"]
	if !validMutationKey(m.key) {
		return nil, false
	}
	if operation == "replace" || operation == "update" || operation == "workstream-set" {
		n, err := strconv.ParseInt(values["--expect-revision"], 10, 64)
		if err != nil {
			return nil, false
		}
		m.revision = n
	}
	if operation == "workstream-set" {
		_, selected := values["--workstream"]
		_, clear := values["--clear"]
		if selected == clear {
			return nil, false
		}
		m.association = client.ReplaceJobWorkstreamRequest{ExpectedRevision: m.revision, WorkstreamID: values["--workstream"]}
		return m, client.ValidateReplaceJobWorkstreamRequest(m.association) == nil
	}
	m.file = values["--file"]
	if len(m.file) > 4096 || !filepath.IsAbs(m.file) {
		return nil, false
	}
	if (operation == "replace" || operation == "update") && (m.revision < 1 || m.revision == math.MaxInt64) {
		return nil, false
	}
	return m, true
}
func prepareProjectMap(ctx context.Context, m *projectMapInvocation) error {
	if m.file == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	invalid := func() error {
		return &client.Error{Code: "invalid_map_file", Message: "Select a readable regular editable map JSON file within 64 KiB.", Action: "Correct the selected file locally and retry the same intended request.", Status: 400}
	}
	before, err := os.Lstat(m.file)
	if err != nil || !before.Mode().IsRegular() || before.Size() > 64<<10 {
		return invalid()
	}
	f, err := os.Open(m.file)
	if err != nil {
		return invalid()
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !actual.Mode().IsRegular() || !os.SameFile(before, actual) || actual.Size() > 64<<10 {
		return invalid()
	}
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return invalid()
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if m.operation == "update" {
		m.changes, err = client.DecodeProjectMapChanges(data)
		if err != nil {
			return invalid()
		}
		return nil
	}
	m.entry, err = client.DecodeProjectMapEditable(data)
	if err != nil {
		return invalid()
	}
	if m.operation == "replace" && client.ValidateReplaceProjectMapEntryRequest(client.ReplaceProjectMapEntryRequest{ExpectedRevision: m.revision, Entry: m.entry}) != nil {
		return invalid()
	}
	return nil
}
func executeProjectMap(ctx context.Context, c *client.Client, m *projectMapInvocation) (any, error) {
	switch m.operation {
	case "create":
		return c.CreateProjectMapEntry(ctx, m.key, m.entry)
	case "update":
		return c.UpdateProjectMapEntry(ctx, m.key, m.id, client.UpdateProjectMapEntryRequest{ExpectedRevision: m.revision, Changes: m.changes})
	case "replace":
		return c.ReplaceProjectMapEntry(ctx, m.key, m.id, client.ReplaceProjectMapEntryRequest{ExpectedRevision: m.revision, Entry: m.entry})
	case "read":
		return c.ProjectMapEntry(ctx, m.id)
	case "list":
		return c.ProjectMapEntries(ctx, m.page, m.filter)
	case "history":
		return c.ProjectMapHistory(ctx, m.id, m.page)
	case "revision":
		return c.ProjectMapRevision(ctx, m.id, m.revision)
	case "workstream":
		return c.JobWorkstream(ctx, m.id)
	case "workstream-set":
		return c.ReplaceJobWorkstream(ctx, m.key, m.id, m.association)
	default:
		return c.JobWorkstreamHistory(ctx, m.id, m.page)
	}
}
