package mcpadapter

// Existing project-management tools expose named typed-client operations only.
// Documents are native operation inputs, never URLs, HTTP verbs or credentials.
import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type existingManagementInput struct {
	Document         json.RawMessage `json:"document,omitempty"`
	Changes          json.RawMessage `json:"changes,omitempty"`
	IdempotencyKey   string          `json:"idempotency_key,omitempty"`
	PreviewSHA256    string          `json:"preview_sha256,omitempty"`
	ExpectedRevision *int64          `json:"expected_revision,omitempty"`
	Revision         int64           `json:"revision,omitempty"`
	Limit            int             `json:"limit,omitempty"`
	Cursor           string          `json:"cursor,omitempty"`
	PullNumber       int64           `json:"pull_number,omitempty"`
	Status           string          `json:"status,omitempty"`
	ObligationID     string          `json:"obligation_id,omitempty"`
	CollectionID     string          `json:"collection_id,omitempty"`
	PageID           string          `json:"page_id,omitempty"`
	SourceKey        string          `json:"source_key,omitempty"`
	PullNumbers      []int64         `json:"pull_numbers,omitempty"`
	WaitSeconds      int             `json:"wait_seconds,omitempty"`
	TargetPullNumber *int64          `json:"target_pull_number,omitempty"`
	Reason           string          `json:"reason,omitempty"`
}
type existingManagementArgs struct {
	name         string
	input        existingManagementInput
	presentation client.ReplacePresentationRequest
	publication  client.PublicationInput
}
type existingManagementSpec struct {
	name, description            string
	fields, required             []string
	write, destructive, external bool
}

var existingManagementSpecs = []existingManagementSpec{
	{"presentation_replace", "Replace this project's presentation using the native {expected_revision,presentation} document. Requires project management and an explicit key; preserves the normal theme/config validation.", []string{"document", "idempotency_key"}, []string{"document", "idempotency_key"}, true, true, false},
	{"presentation_history", "Read one presentation history page; no automatic continuation.", []string{"limit", "cursor"}, nil, false, false, false},
	{"presentation_revision_read", "Read one retained presentation revision.", []string{"revision"}, []string{"revision"}, false, false, false},
	{"publication_configuration_read", "Read this project's selected public publication configuration. Requires instance administration. Private edits are not published implicitly.", nil, nil, false, false, false},
	{"publication_preview", "Preview a complete native publication input {expected_revision,enabled,manifest}; returns its content and preview_sha256, without publishing. Requires instance administration.", []string{"document"}, []string{"document"}, false, false, false},
	{"publication_apply", "Apply the exact approved publication input with its preview_sha256, expected revision and explicit key. Requires instance administration. Makes only explicitly selected public content available.", []string{"document", "preview_sha256", "idempotency_key"}, []string{"document", "preview_sha256", "idempotency_key"}, true, true, false},
	{"publication_unpublish", "Disable the selected project's public publication with current expected_revision and an explicit key. Requires instance administration. Retains private content and publication history.", []string{"expected_revision", "idempotency_key"}, []string{"expected_revision", "idempotency_key"}, true, true, false},
	{"review_import_preview", "Validate and preview a complete native review-import document without applying it or inventing review evidence. Requires project management or instance administration.", []string{"document"}, []string{"document"}, false, false, false},
	{"review_import_apply", "Apply the exact previewed native review-import document with preview_sha256 and explicit key. Requires project management or instance administration; retains original evidence and existing admission guards.", []string{"document", "preview_sha256", "idempotency_key"}, []string{"document", "preview_sha256", "idempotency_key"}, true, true, false},
	{"review_transfer_preview", "Preview one bounded additive native review-history transfer without applying it; no lifetime-project export or provider work. Requires project management or instance administration.", []string{"document"}, []string{"document"}, false, false, false},
	{"review_transfer_apply", "Apply the exact previewed additive native review-history transfer with preview_sha256 and explicit key; does not reset controls or manufacture completion credit. Requires project management or instance administration.", []string{"document", "preview_sha256", "idempotency_key"}, []string{"document", "preview_sha256", "idempotency_key"}, true, true, false},
}

func existingManagementSpecFor(name string) (existingManagementSpec, bool) {
	for _, s := range existingManagementSpecs {
		if s.name == name {
			return s, true
		}
	}
	return existingManagementSpec{}, false
}
func existingManagementTool(name string) bool { _, ok := existingManagementSpecFor(name); return ok }
func existingManagementWrite(name string) bool {
	s, ok := existingManagementSpecFor(name)
	return ok && s.write
}
func existingManagementDestructive(name string) bool {
	s, ok := existingManagementSpecFor(name)
	return ok && s.destructive
}
func existingManagementExternal(name string) bool {
	s, ok := existingManagementSpecFor(name)
	return ok && s.external
}
func existingManagementDefinitions() []definition {
	var out []definition
	for _, s := range existingManagementSpecs {
		properties := map[string]any{}
		for _, name := range s.fields {
			p := map[string]any{"type": "string", "minLength": 1, "maxLength": 128}
			switch name {
			case "document", "changes":
				p = map[string]any{"type": "object", "description": "Strict native operation document. The existing typed client validates its exact fields, values and byte budget before any service call."}
			case "revision":
				p = map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64 - 1)}
			case "expected_revision":
				p = map[string]any{"type": "integer", "minimum": 0, "maximum": int64(math.MaxInt64 - 1)}
			case "pull_number":
				p = map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}
			case "target_pull_number":
				p = map[string]any{"type": "integer", "minimum": 0, "maximum": 2147483647}
			case "reason":
				p = map[string]any{"type": "string", "minLength": 1, "maxLength": 4096}
			case "pull_numbers":
				p = map[string]any{"type": "array", "minItems": 1, "maxItems": 100, "uniqueItems": true, "items": map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}}
			case "limit":
				p = map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
			case "wait_seconds":
				p = map[string]any{"type": "integer", "minimum": 0, "maximum": maxToolWaitSeconds}
			case "preview_sha256":
				p = map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"}
			case "cursor":
				p = map[string]any{"type": "string", "minLength": 1, "maxLength": 4096}
			case "status":
				p = map[string]any{"type": "string", "enum": []string{"open", "resolved"}}
			}
			properties[name] = p
		}
		required := s.required
		if required == nil {
			required = []string{}
		}
		out = append(out, definition{s.name, s.description, map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": required}})
	}
	return out
}
func parseExistingManagementArguments(name string, raw json.RawMessage) (*existingManagementArgs, error) {
	spec, ok := existingManagementSpecFor(name)
	if !ok || len(raw) > (256<<10)+1024 || strictjson.Validate(raw) != nil {
		return nil, invalidArguments()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, invalidArguments()
	}
	allowed := map[string]bool{}
	for _, f := range spec.fields {
		allowed[f] = true
	}
	for k, v := range fields {
		if !allowed[k] || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return nil, invalidArguments()
		}
	}
	for _, k := range spec.required {
		var text string
		if value, exists := fields[k]; exists && json.Unmarshal(value, &text) == nil && text == "" {
			return nil, invalidArguments()
		}
		if _, ok := fields[k]; !ok {
			return nil, invalidArguments()
		}
	}
	a := &existingManagementArgs{name: name}
	if strictjson.Decode(raw, &a.input) != nil {
		return nil, invalidArguments()
	}
	in := &a.input
	if _, ok := fields["idempotency_key"]; ok && !validKey(in.IdempotencyKey) {
		return nil, invalidArguments()
	}
	if _, ok := fields["preview_sha256"]; ok && !managementHash(in.PreviewSHA256) {
		return nil, invalidArguments()
	}
	if in.ExpectedRevision != nil && (*in.ExpectedRevision < 0 || *in.ExpectedRevision == math.MaxInt64) {
		return nil, invalidArguments()
	}
	if _, ok := fields["revision"]; ok && (in.Revision < 1 || in.Revision == math.MaxInt64) {
		return nil, invalidArguments()
	}
	if _, ok := fields["pull_number"]; ok && (in.PullNumber < 1 || in.PullNumber > 2147483647) {
		return nil, invalidArguments()
	}
	if _, ok := fields["limit"]; ok && (in.Limit < 1 || in.Limit > 100) {
		return nil, invalidArguments()
	}
	if _, ok := fields["cursor"]; ok && !validEventCursor(in.Cursor) {
		return nil, invalidArguments()
	}
	if _, present := fields["status"]; present && in.Status != "open" && in.Status != "resolved" {
		return nil, invalidArguments()
	}
	if in.WaitSeconds < 0 || in.WaitSeconds > maxToolWaitSeconds {
		return nil, invalidArguments()
	}
	var err error
	switch name {
	case "presentation_replace":
		err = strictjson.Decode(in.Document, &a.presentation)
		if err == nil {
			err = client.ValidateReplacePresentationRequest(a.presentation)
		}
	case "publication_preview", "publication_apply":
		a.publication, err = client.DecodePublicationInput(in.Document)
		if err == nil && name == "publication_apply" {
			err = client.ValidatePublicationApply(in.IdempotencyKey, in.PreviewSHA256, a.publication)
		}
	case "publication_unpublish":
		err = client.ValidatePublicationUnpublish(in.IdempotencyKey, *in.ExpectedRevision)
	case "review_import_preview":
		err = client.ValidateReviewImportDocument(in.Document)
	case "review_import_apply":
		err = client.ValidateReviewImportApply(in.IdempotencyKey, in.PreviewSHA256, in.Document)
	case "review_transfer_preview":
		err = client.ValidateReviewTransferDocument(in.Document)
	case "review_transfer_apply":
		err = client.ValidateReviewTransferApply(in.IdempotencyKey, in.PreviewSHA256, in.Document)
	}
	if err != nil {
		return nil, invalidArguments()
	}
	return a, nil
}
func managementHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'f' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
func existingManagementBudget(a *existingManagementArgs) time.Duration { return 15 * time.Second }
func executeExistingManagement(ctx context.Context, c *client.Client, a *existingManagementArgs) (any, error) {
	in := a.input
	page := client.PageOptions{Limit: in.Limit, Cursor: in.Cursor}
	switch a.name {
	case "presentation_replace":
		return c.ReplacePresentation(ctx, in.IdempotencyKey, a.presentation)
	case "presentation_history":
		return c.PresentationHistory(ctx, page)
	case "presentation_revision_read":
		return c.PresentationRevision(ctx, in.Revision)
	case "publication_configuration_read":
		return c.Publication(ctx)
	case "publication_preview":
		return c.PreviewPublication(ctx, a.publication)
	case "publication_apply":
		return c.ApplyPublication(ctx, in.IdempotencyKey, in.PreviewSHA256, a.publication)
	case "publication_unpublish":
		return c.Unpublish(ctx, in.IdempotencyKey, *in.ExpectedRevision)
	case "review_import_preview":
		return c.PreviewReviewImport(ctx, in.Document)
	case "review_import_apply":
		return c.ApplyReviewImport(ctx, in.IdempotencyKey, in.PreviewSHA256, in.Document)
	case "review_transfer_preview":
		return c.PreviewReviewTransfer(ctx, in.Document)
	case "review_transfer_apply":
		return c.ApplyReviewTransfer(ctx, in.IdempotencyKey, in.PreviewSHA256, in.Document)
	}
	return nil, invalidArguments()
}
