package mcpadapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
	"github.com/benhook1013/FireController/internal/trackingmodel"
)

// trackingToolCall is one validated tracking tool call. Writes take an
// optional idempotency_key: each is a keyed upsert or set operation, so a
// generated key is safe and a retry converges on the same state.
type trackingToolCall struct {
	name, key, target, kind, state string
	revision                       int64
	expected                       *int64
	item                           client.TrackingItemInput
	itemChanges                    client.TrackingItemChanges
	milestone                      client.TrackingMilestoneInput
	links                          client.TrackingLinkChange
	filter                         client.TrackingItemFilter
	page                           client.PageOptions
	document                       json.RawMessage
}

var trackingTools = map[string]bool{"item_list": true, "item_read": true, "item_upsert": true, "item_update": true, "item_links_change": true, "tracking_batch": true, "tracking_history": true, "tracking_revision_read": true, "milestone_list": true, "milestone_read": true, "milestone_upsert": true, "milestone_update": true}

func trackingTool(name string) bool { return trackingTools[name] }
func trackingWrite(name string) bool {
	switch name {
	case "item_upsert", "item_update", "item_links_change", "tracking_batch", "milestone_upsert", "milestone_update":
		return true
	}
	return false
}

func generatedTrackingKey() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "mcp-tracking-" + hex.EncodeToString(b[:])
}

func trackingArguments(name string, data json.RawMessage) (arguments, error) {
	var args arguments
	if len(data) == 0 {
		data = []byte("{}")
	}
	if len(data) > 2<<20 || strictjson.Validate(data) != nil || bytes.Contains(data, []byte("null")) && hasNull(data) {
		return args, invalidArguments()
	}
	var in struct {
		Key              string                         `json:"key"`
		Kind             string                         `json:"kind"`
		IdempotencyKey   string                         `json:"idempotency_key"`
		ExpectedRevision *int64                         `json:"expected_revision"`
		Revision         int64                          `json:"revision"`
		Item             *client.TrackingItemInput      `json:"item"`
		Milestone        *client.TrackingMilestoneInput `json:"milestone"`
		Changes          json.RawMessage                `json:"changes"`
		Add              []trackingmodel.Link           `json:"add"`
		Remove           []trackingmodel.LinkTarget     `json:"remove"`
		Document         json.RawMessage                `json:"document"`
		MilestoneFilter  string                         `json:"milestone_key"`
		Parent           string                         `json:"parent"`
		Status           []string                       `json:"status"`
		Label            string                         `json:"label"`
		Link             string                         `json:"link"`
		State            string                         `json:"state"`
		Limit            int                            `json:"limit"`
		Cursor           string                         `json:"cursor"`
	}
	if strictjson.Decode(data, &in) != nil {
		return args, invalidArguments()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return args, invalidArguments()
	}
	for field := range fields {
		if !trackingFieldAllowed(name, field) {
			return args, invalidArguments()
		}
	}
	call := &trackingToolCall{name: name, key: in.IdempotencyKey, target: in.Key, kind: in.Kind, expected: in.ExpectedRevision, revision: in.Revision, page: client.PageOptions{Limit: 25, Cursor: in.Cursor}}
	if in.Limit != 0 {
		call.page.Limit = in.Limit
	}
	args.tracking = call
	if trackingWrite(name) {
		if call.key == "" {
			call.key = generatedTrackingKey()
		}
		if !validKey(call.key) {
			return args, invalidArguments()
		}
	}
	needsKey := name != "item_list" && name != "milestone_list" && name != "tracking_batch"
	if needsKey && !client.ValidTrackingKey(call.target) || !needsKey && call.target != "" {
		return args, invalidArguments()
	}
	switch name {
	case "item_list":
		call.filter = client.TrackingItemFilter{Milestone: in.MilestoneFilter, Parent: in.Parent, Label: in.Label, Link: in.Link, Statuses: in.Status}
	case "milestone_list":
		call.state = in.State
	case "item_upsert":
		if in.Item == nil {
			return args, invalidArguments()
		}
		call.item = *in.Item
	case "milestone_upsert":
		if in.Milestone == nil {
			return args, invalidArguments()
		}
		call.milestone = *in.Milestone
	case "item_update":
		if len(in.Changes) == 0 || strictjson.Decode(in.Changes, &call.itemChanges) != nil {
			return args, invalidArguments()
		}
	case "milestone_update":
		if len(in.Changes) == 0 || strictjson.Decode(in.Changes, &call.milestone) != nil {
			return args, invalidArguments()
		}
	case "item_links_change":
		call.links = client.TrackingLinkChange{ExpectedRevision: in.ExpectedRevision, Add: in.Add, Remove: in.Remove}
	case "tracking_batch":
		if len(in.Document) == 0 {
			return args, invalidArguments()
		}
		call.document = in.Document
	case "tracking_history", "tracking_revision_read":
		if in.Kind != "items" && in.Kind != "milestones" || name == "tracking_revision_read" && in.Revision < 1 {
			return args, invalidArguments()
		}
	}
	return args, nil
}

// trackingFieldAllowed keeps each tool's accepted input exactly its schema.
func trackingFieldAllowed(name, field string) bool {
	allowed := map[string][]string{
		"item_list":              {"milestone_key", "parent", "status", "label", "link", "limit", "cursor"},
		"item_read":              {"key"},
		"item_upsert":            {"key", "idempotency_key", "expected_revision", "item"},
		"item_update":            {"key", "idempotency_key", "expected_revision", "changes"},
		"item_links_change":      {"key", "idempotency_key", "expected_revision", "add", "remove"},
		"tracking_batch":         {"idempotency_key", "document"},
		"tracking_history":       {"kind", "key", "limit", "cursor"},
		"tracking_revision_read": {"kind", "key", "revision"},
		"milestone_list":         {"state"},
		"milestone_read":         {"key"},
		"milestone_upsert":       {"key", "idempotency_key", "expected_revision", "milestone"},
		"milestone_update":       {"key", "idempotency_key", "expected_revision", "changes"},
	}
	for _, f := range allowed[name] {
		if f == field {
			return true
		}
	}
	return false
}

func hasNull(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		if token == nil {
			return true
		}
	}
}

func executeTrackingTool(ctx context.Context, c *client.Client, t *trackingToolCall) (any, error) {
	switch t.name {
	case "item_list":
		return c.TrackingItems(ctx, t.page, t.filter)
	case "item_read":
		return c.TrackingItem(ctx, t.target)
	case "item_upsert":
		return c.PutTrackingItem(ctx, t.key, t.target, t.expected, t.item)
	case "item_update":
		return c.PatchTrackingItem(ctx, t.key, t.target, t.expected, t.itemChanges)
	case "item_links_change":
		return c.ChangeTrackingLinks(ctx, t.key, t.target, t.links)
	case "tracking_batch":
		return c.TrackingBatch(ctx, t.key, t.document)
	case "tracking_history":
		return c.TrackingHistory(ctx, t.kind, t.target, t.page)
	case "tracking_revision_read":
		if t.kind == "milestones" {
			return c.TrackingMilestoneRevision(ctx, t.target, t.revision)
		}
		return c.TrackingItemRevision(ctx, t.target, t.revision)
	case "milestone_list":
		return c.TrackingMilestones(ctx, t.state)
	case "milestone_read":
		return c.TrackingMilestone(ctx, t.target)
	case "milestone_upsert":
		return c.PutTrackingMilestone(ctx, t.key, t.target, t.expected, t.milestone)
	default:
		return c.PatchTrackingMilestone(ctx, t.key, t.target, t.expected, t.milestone)
	}
}

func trackingDefinitions() []definition {
	object := func(properties map[string]any, required ...string) map[string]any {
		value := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			value["required"] = required
		}
		return value
	}
	str := func(max int, description string) map[string]any {
		v := map[string]any{"type": "string", "maxLength": max}
		if description != "" {
			v["description"] = description
		}
		return v
	}
	key := map[string]any{"type": "string", "pattern": `^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`, "description": "Caller-chosen stable key, unique per project, e.g. v1 or GR-2.1."}
	idem := str(128, "Optional; a fresh key is generated when omitted. Writes are keyed upserts, so retries converge.")
	guard := map[string]any{"type": "integer", "minimum": 0, "description": "Optional revision guard. Omit for last-write-wins; 0 means create only."}
	status := map[string]any{"type": "string", "enum": trackingmodel.Statuses}
	labels := map[string]any{"type": "array", "items": str(64, ""), "maxItems": 16}
	position := map[string]any{"type": "integer", "minimum": 0, "maximum": 1000000}
	link := object(map[string]any{"type": map[string]any{"type": "string", "enum": trackingmodel.LinkTypes}, "ref": str(2048, "job/route/note ID, item key, URL, or pr NUMBER | #NUMBER | owner/name#NUMBER (bare numbers use the project repository)."), "role": map[string]any{"type": "string", "enum": trackingmodel.LinkRoles}, "label": str(120, "")}, "type", "ref", "role")
	target := object(map[string]any{"type": map[string]any{"type": "string", "enum": trackingmodel.LinkTypes}, "ref": str(2048, ""), "role": map[string]any{"type": "string", "description": "Optional; omit to remove every role for this target."}}, "type", "ref")
	itemFields := map[string]any{"title": str(240, ""), "description": str(2048, "Short Markdown; not a proof log."), "status": status, "milestone": str(64, "Existing milestone key; empty clears."), "parent": str(64, "Existing parent item key; empty clears."), "labels": labels, "revisit_when": str(1024, "When to reconsider a deferred item."), "position": position}
	item := object(map[string]any{}, "title")
	for k, v := range itemFields {
		item["properties"].(map[string]any)[k] = v
	}
	item["properties"].(map[string]any)["links"] = map[string]any{"type": "array", "items": link, "maxItems": 64, "description": "Omit to keep current links; supply to replace them."}
	changes := object(map[string]any{"add_labels": labels, "remove_labels": labels})
	for k, v := range itemFields {
		changes["properties"].(map[string]any)[k] = v
	}
	changes["minProperties"] = 1
	milestone := object(map[string]any{"title": str(240, ""), "description": str(8192, ""), "state": map[string]any{"type": "string", "enum": []string{"open", "closed"}}, "target_date": str(10, "YYYY-MM-DD or empty."), "position": position})
	milestoneCreate := object(milestone["properties"].(map[string]any), "title")
	milestone["minProperties"] = 1
	page := map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}, "cursor": str(4096, "")}
	kind := map[string]any{"type": "string", "enum": []string{"items", "milestones"}}
	with := func(base map[string]any, extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	return []definition{
		{"milestone_list", "List milestones with item counts by status. Optional state open/closed. No provider work.", object(map[string]any{"state": map[string]any{"type": "string", "enum": []string{"open", "closed"}}})},
		{"milestone_read", "Read one milestone with item counts by status and up to 100 open items (planned/active/blocked/deferred).", object(map[string]any{"key": key}, "key")},
		{"milestone_upsert", "Declare one milestone by key (create or replace its fields). State defaults to open.", object(map[string]any{"key": key, "idempotency_key": idem, "expected_revision": guard, "milestone": milestoneCreate}, "key", "milestone")},
		{"milestone_update", "Change selected milestone fields, e.g. state closed. Title/description changes need expected_revision.", object(map[string]any{"key": key, "idempotency_key": idem, "expected_revision": guard, "changes": milestone}, "key", "changes")},
		{"item_list", "List compact item rows ordered by position then key. Filters combine: milestone_key, parent, status (any of), label, link TYPE:REF such as pr:42 or job:JOB_ID.", object(with(page, map[string]any{"milestone_key": key, "parent": key, "status": map[string]any{"type": "array", "items": status, "maxItems": 6}, "label": str(64, ""), "link": str(2100, "TYPE:REF")}))},
		{"item_read", "Read one full item with its links and its direct children's counts by status.", object(map[string]any{"key": key}, "key")},
		{"item_upsert", "Declare one item by key. Status defaults to planned. Omitted links keep the current links. Milestone, parent and linked job/note/route/item must exist.", object(map[string]any{"key": key, "idempotency_key": idem, "expected_revision": guard, "item": item}, "key", "item")},
		{"item_update", "Change selected item fields: the cheap path for status, milestone, parent, labels (add_labels/remove_labels), revisit_when, position. Title/description changes need expected_revision. Status never changes automatically.", object(map[string]any{"key": key, "idempotency_key": idem, "expected_revision": guard, "changes": changes}, "key", "changes")},
		{"item_links_change", "Add and/or remove typed links as one idempotent set change. Roles: delivers, proof, blocks, related. Proof is a done item plus a proof link.", object(map[string]any{"key": key, "idempotency_key": idem, "expected_revision": guard, "add": map[string]any{"type": "array", "items": link, "maxItems": 64}, "remove": map[string]any{"type": "array", "items": target, "maxItems": 64}}, "key")},
		{"tracking_batch", "Declare up to 20 milestones then up to 100 items in one transaction; each record follows the upsert rules, so reapplying the same document is a no-op. document is {milestones:[{key,expected_revision?,milestone}], items:[{key,expected_revision?,item}]}.", object(map[string]any{"idempotency_key": idem, "document": map[string]any{"type": "object"}}, "document")},
		{"tracking_history", "Read one newest-first page of an item's or milestone's revision summaries.", object(with(page, map[string]any{"kind": kind, "key": key}), "kind", "key")},
		{"tracking_revision_read", "Read one captured item or milestone revision with its actor and time.", object(map[string]any{"kind": kind, "key": key, "revision": map[string]any{"type": "integer", "minimum": 1}}, "kind", "key", "revision")},
	}
}
