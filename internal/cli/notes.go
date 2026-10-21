package cli

import (
	"context"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
)

type noteInvocation struct {
	operation, id, key     string
	revision               int64
	create                 client.CreateNoteRequest
	update                 client.UpdateNoteRequest
	filter                 client.NoteFilter
	page                   client.PageOptions
	defaultWorker          bool
	allWorkers, directOnly bool
}

func (n *noteInvocation) write() bool { return n.operation == "add" || n.operation == "update" }
func parseNotes(operation string, args []string) (*noteInvocation, bool) {
	n := &noteInvocation{operation: operation, page: client.PageOptions{Limit: 20}}
	allowed := map[string]bool{}
	booleans := map[string]bool{}
	switch operation {
	case "add", "update":
		for _, k := range []string{"--key", "--worker", "--job", "--phase", "--kind", "--body", "--body-file"} {
			allowed[k] = true
		}
		booleans["--clear-worker"] = true
		if operation == "update" {
			allowed["--expect-revision"], allowed["--status"], allowed["--reason"] = true, true, true
			booleans["--clear-job"], booleans["--clear-phase"] = true, true
		}
	case "list":
		for _, k := range []string{"--worker", "--job", "--phase", "--kind", "--status", "--limit", "--cursor"} {
			allowed[k] = true
		}
		booleans["--all-workers"], booleans["--direct-only"] = true, true
	case "history":
		allowed["--limit"], allowed["--cursor"] = true, true
	case "read", "revision":
	default:
		return nil, false
	}
	values := map[string]string{}
	var positions []string
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			positions = append(positions, args[i])
			continue
		}
		flag, value, assigned := strings.Cut(args[i], "=")
		if _, duplicate := values[flag]; duplicate {
			return nil, false
		}
		if booleans[flag] {
			if assigned {
				return nil, false
			}
			values[flag] = "true"
			continue
		}
		if !allowed[flag] {
			return nil, false
		}
		if !assigned {
			i++
			if i >= len(args) {
				return nil, false
			}
			value = args[i]
		}
		if !utf8.ValidString(value) {
			return nil, false
		}
		values[flag] = value
	}
	expected := 0
	if operation == "read" || operation == "history" || operation == "update" {
		expected = 1
	}
	if operation == "revision" {
		expected = 2
	}
	if len(positions) != expected {
		return nil, false
	}
	if expected > 0 {
		n.id = positions[0]
		if !validResourceID(n.id) {
			return nil, false
		}
	}
	if operation == "revision" {
		v, e := strconv.ParseInt(positions[1], 10, 64)
		if e != nil || v < 1 {
			return nil, false
		}
		n.revision = v
	}
	for _, flag := range []string{"--worker", "--job", "--phase", "--kind", "--status"} {
		if v, ok := values[flag]; ok && v == "" {
			return nil, false
		}
	}
	if value, ok := values["--limit"]; ok {
		v, e := strconv.Atoi(value)
		if e != nil || v < 1 || v > 100 {
			return nil, false
		}
		n.page.Limit = v
	}
	if value, ok := values["--cursor"]; ok {
		if len(value) == 0 || len(value) > 4096 || !utf8.ValidString(value) || value != strings.TrimSpace(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return nil, false
		}
		n.page.Cursor = value
	}
	if operation == "list" {
		n.allWorkers = values["--all-workers"] == "true"
		n.directOnly = values["--direct-only"] == "true"
		_, explicitWorker := values["--worker"]
		if n.allWorkers && (n.directOnly || explicitWorker) {
			return nil, false
		}
		n.filter = client.NoteFilter{JobID: values["--job"], Phase: values["--phase"], Kind: values["--kind"], Status: values["--status"]}
		if !n.allWorkers {
			n.defaultWorker = !explicitWorker
			n.filter.WorkerID = values["--worker"]
			if n.defaultWorker {
				n.filter.WorkerID = "bound-worker"
			}
			n.filter.IncludeAssignedJobs = !n.directOnly
		}
		return n, client.ValidateNoteFilter(n.filter) == nil
	}
	if !n.write() {
		return n, true
	}
	n.key = values["--key"]
	if !client.ValidMutationKey(n.key) {
		return nil, false
	}
	for _, pair := range [][2]string{{"--worker", "--clear-worker"}, {"--job", "--clear-job"}, {"--phase", "--clear-phase"}} {
		_, a := values[pair[0]]
		_, b := values[pair[1]]
		if a && b {
			return nil, false
		}
	}
	_, body := values["--body"]
	_, file := values["--body-file"]
	if body && file {
		return nil, false
	}
	if operation == "add" {
		if !body && !file {
			return nil, false
		}
		n.defaultWorker = values["--clear-worker"] != "true"
		if _, given := values["--worker"]; given {
			n.defaultWorker = false
		}
		worker := values["--worker"]
		if n.defaultWorker {
			worker = "bound-worker"
		}
		n.create = client.CreateNoteRequest{WorkerID: worker, JobID: values["--job"], Phase: values["--phase"], Kind: values["--kind"], Body: values["--body"]}
		if n.create.Kind == "" {
			n.create.Kind = "reminder"
		}
		if file {
			n.create.Body = "file validation"
		}
		if client.ValidateCreateNoteRequest(n.create) != nil {
			return nil, false
		}
	} else {
		revision, e := strconv.ParseInt(values["--expect-revision"], 10, 64)
		if e != nil {
			return nil, false
		}
		n.update.ExpectedRevision = revision
		for flag, dst := range map[string]**string{"--worker": &n.update.WorkerID, "--job": &n.update.JobID, "--phase": &n.update.Phase, "--kind": &n.update.Kind, "--body": &n.update.Body, "--status": &n.update.Status, "--reason": &n.update.StatusReason} {
			if value, ok := values[flag]; ok {
				v := value
				*dst = &v
			}
		}
		for flag, dst := range map[string]**string{"--clear-worker": &n.update.WorkerID, "--clear-job": &n.update.JobID, "--clear-phase": &n.update.Phase} {
			if values[flag] == "true" {
				empty := ""
				*dst = &empty
			}
		}
		if file {
			placeholder := "file validation"
			n.update.Body = &placeholder
		}
		if client.ValidateUpdateNoteRequest(n.update) != nil {
			return nil, false
		}
	}
	if file {
		value, ok := readNoteBodyFile(values["--body-file"])
		if !ok {
			return nil, false
		}
		if operation == "add" {
			n.create.Body = value
		} else {
			n.update.Body = &value
		}
	}
	return n, operation == "add" && client.ValidateCreateNoteRequest(n.create) == nil || operation == "update" && client.ValidateUpdateNoteRequest(n.update) == nil
}
func readNoteBodyFile(path string) (string, bool) {
	return readPrivateTextFile(path, 16<<10)
}
func executeNotes(ctx context.Context, c *client.Client, n *noteInvocation) (any, error) {
	switch n.operation {
	case "add":
		in := n.create
		if n.defaultWorker {
			in.WorkerID = c.Identity().WorkerID
		}
		return c.CreateNote(ctx, n.key, in)
	case "update":
		return c.UpdateNote(ctx, n.key, n.id, n.update)
	case "read":
		return c.Note(ctx, n.id)
	case "revision":
		return c.NoteRevision(ctx, n.id, n.revision)
	case "history":
		return c.NoteHistory(ctx, n.id, n.page)
	default:
		filter := n.filter
		if n.defaultWorker {
			filter.WorkerID = c.Identity().WorkerID
		}
		return c.Notes(ctx, n.page, filter)
	}
}
