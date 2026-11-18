package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/trackingmodel"
)

// trackingInvocation is one parsed `items` or `milestones` command. Writes
// default to a fresh idempotency key: every tracking write is a keyed upsert
// or set operation, so a retry with a new key converges on the same state.
type trackingInvocation struct {
	command, operation, target, key, file string
	revision                              int64
	expected                              *int64
	item                                  client.TrackingItemInput
	itemChanges                           client.TrackingItemChanges
	milestone                             client.TrackingMilestoneInput
	links                                 client.TrackingLinkChange
	filter                                client.TrackingItemFilter
	state                                 string
	page                                  client.PageOptions
	document                              json.RawMessage
}

func trackingCommand(command string) bool { return command == "items" || command == "milestones" }

func (t *trackingInvocation) write() bool {
	switch t.operation {
	case "create", "set", "link", "unlink", "upsert":
		return true
	}
	return false
}

func newTrackingKey() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "fc-tracking-" + hex.EncodeToString(b[:])
}

func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseTracking(command, operation string, args []string) (*trackingInvocation, bool) {
	t := &trackingInvocation{command: command, operation: operation, page: client.PageOptions{Limit: 25}}
	flags := map[string]bool{}
	switches := map[string]bool{}
	positions := 1
	write := []string{"--key", "--expect-revision"}
	itemFields := []string{"--title", "--description", "--status", "--milestone", "--parent", "--labels", "--revisit-when", "--position"}
	milestoneFields := []string{"--title", "--description", "--target-date", "--position"}
	switch command + " " + operation {
	case "items list":
		positions = 0
		for _, f := range []string{"--milestone", "--parent", "--status", "--label", "--link", "--limit", "--cursor"} {
			flags[f] = true
		}
	case "milestones list":
		positions = 0
		flags["--state"] = true
	case "items read", "milestones read":
	case "items history", "milestones history":
		flags["--limit"], flags["--cursor"] = true, true
	case "items revision", "milestones revision":
		positions = 2
	case "items create":
		for _, f := range append(itemFields, "--key") {
			flags[f] = true
		}
	case "items set":
		for _, f := range append(append(itemFields, write...), "--add-labels", "--remove-labels") {
			flags[f] = true
		}
		switches["--clear-milestone"], switches["--clear-parent"] = true, true
	case "items link", "items unlink":
		positions = 2
		for _, f := range append(write, "--role", "--label") {
			flags[f] = true
		}
	case "items upsert":
		positions = 0
		flags["--file"], flags["--key"] = true, true
	case "milestones create":
		for _, f := range append(milestoneFields, "--key") {
			flags[f] = true
		}
	case "milestones set":
		for _, f := range append(append(milestoneFields, write...), "--state") {
			flags[f] = true
		}
	default:
		return nil, false
	}
	if operation == "unlink" {
		delete(flags, "--label")
	}
	values := map[string]string{}
	set := map[string]bool{}
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			positional = append(positional, arg)
			continue
		}
		flag, value, assigned := strings.Cut(arg, "=")
		if switches[flag] && !assigned {
			if set[flag] {
				return nil, false
			}
			set[flag] = true
			continue
		}
		if !flags[flag] {
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
		empty := value == "" && (flag == "--description" || flag == "--revisit-when" || flag == "--target-date" || flag == "--labels" || flag == "--label")
		if value == "" && !empty || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return nil, false
		}
		values[flag] = value
	}
	if len(positional) != positions {
		return nil, false
	}
	if positions > 0 {
		t.target = positional[0]
		if !client.ValidTrackingKey(t.target) {
			return nil, false
		}
	}
	str := func(flag string) *string {
		if v, ok := values[flag]; ok {
			return &v
		}
		return nil
	}
	num := func(flag string, min, max int64) (*int64, bool) {
		v, ok := values[flag]
		if !ok {
			return nil, true
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < min || n > max || strconv.FormatInt(n, 10) != v {
			return nil, false
		}
		return &n, true
	}
	var ok bool
	if t.expected, ok = num("--expect-revision", 1, 1<<53); !ok {
		return nil, false
	}
	position, ok := num("--position", 0, 1000000)
	if !ok {
		return nil, false
	}
	if limit, ok := num("--limit", 1, 100); !ok {
		return nil, false
	} else if limit != nil {
		t.page.Limit = int(*limit)
	}
	t.page.Cursor = values["--cursor"]
	t.key = values["--key"]
	if t.write() {
		if t.key == "" {
			t.key = newTrackingKey()
		}
		if !validMutationKey(t.key) {
			return nil, false
		}
	}
	switch operation {
	case "list":
		if command == "milestones" {
			t.state = values["--state"]
			return t, t.state == "" || t.state == "open" || t.state == "closed"
		}
		t.filter = client.TrackingItemFilter{Milestone: values["--milestone"], Parent: values["--parent"], Label: values["--label"], Link: values["--link"], Statuses: splitList(values["--status"])}
		return t, t.filter.Link == "" || strings.Contains(t.filter.Link, ":")
	case "revision":
		n, err := strconv.ParseInt(positional[1], 10, 64)
		if err != nil || n < 1 || strconv.FormatInt(n, 10) != positional[1] {
			return nil, false
		}
		t.revision = n
	case "create":
		zero := int64(0)
		t.expected = &zero
		if command == "milestones" {
			t.milestone = client.TrackingMilestoneInput{Title: str("--title"), Description: str("--description"), TargetDate: str("--target-date"), Position: position}
			return t, t.milestone.Title != nil
		}
		t.item = client.TrackingItemInput{Title: str("--title"), Description: str("--description"), Status: str("--status"), Milestone: str("--milestone"), Parent: str("--parent"), RevisitWhen: str("--revisit-when"), Position: position}
		if v, ok := values["--labels"]; ok {
			labels := splitList(v)
			t.item.Labels = &labels
		}
		return t, t.item.Title != nil
	case "set":
		if command == "milestones" {
			t.milestone = client.TrackingMilestoneInput{Title: str("--title"), Description: str("--description"), State: str("--state"), TargetDate: str("--target-date"), Position: position}
			m := t.milestone
			changed := m.Title != nil || m.Description != nil || m.State != nil || m.TargetDate != nil || m.Position != nil
			return t, changed && (m.Title == nil && m.Description == nil || t.expected != nil)
		}
		c := client.TrackingItemChanges{Title: str("--title"), Description: str("--description"), Status: str("--status"), Milestone: str("--milestone"), Parent: str("--parent"), RevisitWhen: str("--revisit-when"), Position: position, AddLabels: splitList(values["--add-labels"]), RemoveLabels: splitList(values["--remove-labels"])}
		if v, ok := values["--labels"]; ok {
			labels := splitList(v)
			c.Labels = &labels
		}
		empty := ""
		if set["--clear-milestone"] {
			if c.Milestone != nil {
				return nil, false
			}
			c.Milestone = &empty
		}
		if set["--clear-parent"] {
			if c.Parent != nil {
				return nil, false
			}
			c.Parent = &empty
		}
		t.itemChanges = c
		changed := c.Title != nil || c.Description != nil || c.Status != nil || c.Milestone != nil || c.Parent != nil || c.Labels != nil || len(c.AddLabels) > 0 || len(c.RemoveLabels) > 0 || c.RevisitWhen != nil || c.Position != nil
		return t, changed && (c.Title == nil && c.Description == nil || t.expected != nil)
	case "link", "unlink":
		kind, ref, found := strings.Cut(positional[1], ":")
		if !found || kind == "" || ref == "" {
			return nil, false
		}
		t.links.ExpectedRevision = t.expected
		if operation == "unlink" {
			t.links.Remove = []trackingmodel.LinkTarget{{Type: kind, Ref: ref, Role: values["--role"]}}
			return t, true
		}
		role := values["--role"]
		if role == "" {
			role = "delivers"
		}
		t.links.Add = []trackingmodel.Link{{Type: kind, Ref: ref, Role: role, Label: values["--label"]}}
	case "upsert":
		t.file = values["--file"]
		return t, len(t.file) <= 4096 && filepath.IsAbs(t.file)
	}
	return t, true
}

func prepareTracking(ctx context.Context, t *trackingInvocation) error {
	if t.file == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	invalid := func() error {
		return &client.Error{Code: "invalid_tracking_file", Message: "Select a readable regular JSON batch file within 2 MiB holding milestones and/or items.", Action: "Correct the file locally and retry; reapplying the same file is a no-op.", Status: 400}
	}
	info, err := os.Lstat(t.file)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 2<<20 {
		return invalid()
	}
	f, err := os.Open(t.file)
	if err != nil {
		return invalid()
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (2<<20)+1))
	if err != nil {
		return invalid()
	}
	if t.document, err = client.DecodeTrackingBatch(data); err != nil {
		return invalid()
	}
	return nil
}

func executeTracking(ctx context.Context, c *client.Client, t *trackingInvocation) (any, error) {
	kind := t.command
	switch t.operation {
	case "list":
		if kind == "milestones" {
			return c.TrackingMilestones(ctx, t.state)
		}
		return c.TrackingItems(ctx, t.page, t.filter)
	case "read":
		if kind == "milestones" {
			return c.TrackingMilestone(ctx, t.target)
		}
		return c.TrackingItem(ctx, t.target)
	case "history":
		return c.TrackingHistory(ctx, kind, t.target, t.page)
	case "revision":
		if kind == "milestones" {
			return c.TrackingMilestoneRevision(ctx, t.target, t.revision)
		}
		return c.TrackingItemRevision(ctx, t.target, t.revision)
	case "create":
		if kind == "milestones" {
			return c.PutTrackingMilestone(ctx, t.key, t.target, t.expected, t.milestone)
		}
		return c.PutTrackingItem(ctx, t.key, t.target, t.expected, t.item)
	case "set":
		if kind == "milestones" {
			return c.PatchTrackingMilestone(ctx, t.key, t.target, t.expected, t.milestone)
		}
		return c.PatchTrackingItem(ctx, t.key, t.target, t.expected, t.itemChanges)
	case "link", "unlink":
		return c.ChangeTrackingLinks(ctx, t.key, t.target, t.links)
	default:
		return c.TrackingBatch(ctx, t.key, t.document)
	}
}
