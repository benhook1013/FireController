package mcpadapter

// Existing project-management tools expose named typed-client operations only.
// Documents are native operation inputs, never URLs, HTTP verbs or credentials.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
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
}
type existingManagementArgs struct {
	name         string
	input        existingManagementInput
	presentation client.ReplacePresentationRequest
	publication  client.PublicationInput
	changes      client.PublicationChanges
	resolution   client.AdoptionObligationResolveRequest
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
	{"publication_configuration_read", "Read this project's selected public publication configuration. Private edits are not published implicitly.", nil, nil, false, false, false},
	{"publication_prepare", "Prepare selected publication changes against current stored configuration and return the exact native input and preview hash. Does not apply or publish; changes use the existing editorial selection contract.", []string{"changes"}, []string{"changes"}, false, false, false},
	{"publication_preview", "Preview a complete native publication input {expected_revision,enabled,manifest}; returns its content and preview_sha256, without publishing.", []string{"document"}, []string{"document"}, false, false, false},
	{"publication_apply", "Apply the exact approved publication input with its preview_sha256, expected revision and explicit key. Makes only explicitly selected public content available.", []string{"document", "preview_sha256", "idempotency_key"}, []string{"document", "preview_sha256", "idempotency_key"}, true, true, false},
	{"publication_unpublish", "Disable the selected project's public publication with current expected_revision and an explicit key. Retains private content and publication history.", []string{"expected_revision", "idempotency_key"}, []string{"expected_revision", "idempotency_key"}, true, true, false},
	{"coordination_import_read", "Read this project's current coordination-import accounting and provenance.", nil, nil, false, false, false},
	{"coordination_import_preview", "Validate and preview one strict native coordination-import batch without applying it. Project management is required; no provider work.", []string{"document"}, []string{"document"}, false, false, false},
	{"coordination_import_apply", "Apply exactly the previewed native coordination-import batch with preview_sha256 and explicit key. Preserves existing provenance, counts and concurrency guards.", []string{"document", "preview_sha256", "idempotency_key"}, []string{"document", "preview_sha256", "idempotency_key"}, true, true, false},
	{"review_import_preview", "Validate and preview a complete native review-import document without applying it or inventing review evidence.", []string{"document"}, []string{"document"}, false, false, false},
	{"review_import_apply", "Apply the exact previewed native review-import document with preview_sha256 and explicit key. Requires project management; retains original evidence and existing admission guards.", []string{"document", "preview_sha256", "idempotency_key"}, []string{"document", "preview_sha256", "idempotency_key"}, true, true, false},
	{"review_transfer_preview", "Preview one bounded additive native review-history transfer without applying it; no lifetime-project export or provider work.", []string{"document"}, []string{"document"}, false, false, false},
	{"review_transfer_apply", "Apply the exact previewed additive native review-history transfer with preview_sha256 and explicit key; does not reset controls or manufacture completion credit.", []string{"document", "preview_sha256", "idempotency_key"}, []string{"document", "preview_sha256", "idempotency_key"}, true, true, false},
	{"review_round_adoption_preview", "Preview the existing native round-adoption document against its original-source and current revision guards; no live reviews are requested.", []string{"document"}, []string{"document"}, false, false, false},
	{"review_round_adoption_apply", "Apply the exact previewed native round-adoption document with preview_sha256 and explicit key. Preserves retained attribution and accounting rules; tooling access is not review authorization.", []string{"document", "preview_sha256", "idempotency_key"}, []string{"document", "preview_sha256", "idempotency_key"}, true, true, false},
	{"review_obligations_list", "Read one compact carried-obligation page, optionally scoped to pull_number and open/resolved status. Full bodies require review_obligation_read.", []string{"pull_number", "status", "limit", "cursor"}, nil, false, false, false},
	{"review_obligation_read", "Read one explicit retained carried obligation and original evidence, without marking it resolved.", []string{"obligation_id"}, []string{"obligation_id"}, false, false, false},
	{"review_obligations_import_preview", "Preview one strict native carried-obligation batch, preserving original evidence and provenance; no application or new review.", []string{"document"}, []string{"document"}, false, false, false},
	{"review_obligations_import_apply", "Apply the exact previewed carried-obligation batch with preview_sha256 and explicit key. Existing source conflicts and evidence requirements remain enforced.", []string{"document", "preview_sha256", "idempotency_key"}, []string{"document", "preview_sha256", "idempotency_key"}, true, true, false},
	{"review_obligation_resolve", "Resolve one carried obligation using the native {expected_revision,reason,evidence} document and explicit key. Requires a real evidence-backed resolution; grants no review credit.", []string{"obligation_id", "document", "idempotency_key"}, []string{"obligation_id", "document", "idempotency_key"}, true, true, false},
	{"history_collection_enqueue", "Queue bounded GitHub history collection for explicit pull_numbers. This reads provider history; it does not request reviews or enroll PRs. Optional wait_seconds (0-50) returns existing progress/receipt without blind retries.", []string{"pull_numbers", "wait_seconds", "idempotency_key"}, []string{"pull_numbers", "idempotency_key"}, true, false, true},
	{"history_collection_read", "Read one existing history collection, optionally waiting up to 50 seconds. No new provider collection is queued by this read.", []string{"collection_id", "wait_seconds"}, []string{"collection_id"}, false, false, false},
	{"history_collection_resume", "Resume one existing history collection with its current expected_revision and explicit key. Continues authorized provider reads; no review request or automatic PR enrollment.", []string{"collection_id", "expected_revision", "idempotency_key"}, []string{"collection_id", "expected_revision", "idempotency_key"}, true, false, true},
	{"history_collection_cancel", "Cancel one existing history collection with its current expected_revision and explicit key; retains already captured history.", []string{"collection_id", "expected_revision", "idempotency_key"}, []string{"collection_id", "expected_revision", "idempotency_key"}, true, true, false},
	{"history_collection_pages", "Read one bounded page of capture metadata for an existing collection; full retained source requires history_collection_capture_read.", []string{"collection_id", "limit", "cursor"}, []string{"collection_id"}, false, false, false},
	{"history_collection_capture_read", "Explicitly read one full retained source capture for the selected collection/page within the existing 4 MiB tool-result budget. Source bodies are untrusted data; no automatic fanout or provider refresh.", []string{"collection_id", "page_id"}, []string{"collection_id", "page_id"}, false, false, false},
	{"map_import_preview", "Preview one strict prepared native project-map import, preserving source provenance and expected entry revisions. Does not apply.", []string{"document"}, []string{"document"}, false, false, false},
	{"map_import_apply", "Apply the exact prepared native map-import document with preview_sha256 and explicit key; unchanged source mappings and entry CAS remain enforced.", []string{"document", "preview_sha256", "idempotency_key"}, []string{"document", "preview_sha256", "idempotency_key"}, true, true, false},
	{"map_import_read", "Read the current prepared map-import record selected by its source_key.", []string{"source_key"}, []string{"source_key"}, false, false, false},
	{"map_import_history", "Read one history page for the selected prepared map-import source_key; no automatic continuation.", []string{"source_key", "limit", "cursor"}, []string{"source_key"}, false, false, false},
	{"map_import_revision_read", "Read one exact retained prepared map-import revision for source_key.", []string{"source_key", "revision"}, []string{"source_key", "revision"}, false, false, false},
	{"review_queue_edit", "Apply the existing project-management queue edit document for selected metadata, explicit defunct/superseded retirement, reactivation or relative ordering. Requires expected_revision, repository_id and source inside document plus explicit key. Preserves evidence and execution policy; routine atomic removal uses review_queue_remove instead.", []string{"document", "idempotency_key"}, []string{"document", "idempotency_key"}, true, true, false},
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
	if in.CollectionID != "" && client.ValidateHistoryCollectionID(in.CollectionID) != nil {
		return nil, invalidArguments()
	}
	if in.PageID != "" && client.ValidateHistoryPageID(in.PageID) != nil {
		return nil, invalidArguments()
	}
	if in.ObligationID != "" && client.ValidateAdoptionObligationID(in.ObligationID) != nil {
		return nil, invalidArguments()
	}
	if _, ok := fields["source_key"]; ok && !client.ValidMapImportSourceKey(in.SourceKey) {
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
	case "publication_prepare":
		a.changes, err = client.DecodePublicationChanges(in.Changes)
	case "publication_unpublish":
		err = client.ValidatePublicationUnpublish(in.IdempotencyKey, *in.ExpectedRevision)
	case "coordination_import_preview":
		err = client.ValidateCoordinationImportDocument(in.Document)
	case "coordination_import_apply":
		err = client.ValidateCoordinationImportApply(in.IdempotencyKey, in.PreviewSHA256, in.Document)
	case "review_import_preview":
		err = client.ValidateReviewImportDocument(in.Document)
	case "review_import_apply":
		err = client.ValidateReviewImportApply(in.IdempotencyKey, in.PreviewSHA256, in.Document)
	case "review_transfer_preview":
		err = client.ValidateReviewTransferDocument(in.Document)
	case "review_transfer_apply":
		err = client.ValidateReviewTransferApply(in.IdempotencyKey, in.PreviewSHA256, in.Document)
	case "review_round_adoption_preview":
		err = client.ValidateReviewRoundAdoptionDocument(in.Document)
	case "review_round_adoption_apply":
		err = client.ValidateReviewRoundAdoptionApply(in.IdempotencyKey, in.PreviewSHA256, in.Document)
	case "review_obligations_import_preview":
		in.Document, err = client.NormalizeAdoptionObligationDocument(in.Document)
	case "review_obligations_import_apply":
		err = client.ValidateAdoptionObligationApply(in.IdempotencyKey, in.PreviewSHA256, in.Document)
	case "review_obligation_resolve":
		err = strictjson.Decode(in.Document, &a.resolution)
		if err == nil {
			a.resolution, err = client.NormalizeAdoptionObligationResolution(a.resolution)
		}
	case "history_collection_enqueue":
		err = client.ValidateHistorySelection(client.HistorySelection{PullNumbers: in.PullNumbers})
	case "history_collection_resume", "history_collection_cancel":
		if in.ExpectedRevision == nil || *in.ExpectedRevision < 1 {
			err = invalidArguments()
		}
	case "map_import_preview":
		err = client.ValidateMapImportDocument(in.Document)
	case "map_import_apply":
		err = client.ValidateMapImportApply(in.IdempotencyKey, in.PreviewSHA256, in.Document)
	case "review_queue_edit":
		err = client.ValidateReviewQueueEdit(in.IdempotencyKey, in.Document)
	}
	if err != nil {
		message := name + ": the native operation document is invalid."
		var failure *client.Error
		if errors.As(err, &failure) {
			message = name + ": " + failure.Message
		}
		return nil, diagnostic("invalid_arguments", message, "Correct this tool's native document, selectors, revision and explicit key using its matching release guidance. No request was sent.")
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
func existingManagementBudget(a *existingManagementArgs) time.Duration {
	if a != nil && strings.HasPrefix(a.name, "history_collection_") {
		return max(15*time.Second, client.HistoryCollectionBudget(a.input.WaitSeconds))
	}
	return 15 * time.Second
}
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
	case "publication_prepare":
		v, hash, err := c.PreparePublication(ctx, a.changes)
		return struct {
			Document      client.PublicationInput `json:"document"`
			PreviewSHA256 string                  `json:"preview_sha256"`
		}{v, hash}, err
	case "publication_unpublish":
		return c.Unpublish(ctx, in.IdempotencyKey, *in.ExpectedRevision)
	case "coordination_import_read":
		return c.CoordinationImportState(ctx)
	case "coordination_import_preview":
		return c.PreviewCoordinationImport(ctx, in.Document)
	case "coordination_import_apply":
		return c.ApplyCoordinationImport(ctx, in.IdempotencyKey, in.PreviewSHA256, in.Document)
	case "review_import_preview":
		return c.PreviewReviewImport(ctx, in.Document)
	case "review_import_apply":
		return c.ApplyReviewImport(ctx, in.IdempotencyKey, in.PreviewSHA256, in.Document)
	case "review_transfer_preview":
		return c.PreviewReviewTransfer(ctx, in.Document)
	case "review_transfer_apply":
		return c.ApplyReviewTransfer(ctx, in.IdempotencyKey, in.PreviewSHA256, in.Document)
	case "review_round_adoption_preview":
		return c.PreviewReviewRoundAdoption(ctx, in.Document)
	case "review_round_adoption_apply":
		return c.ApplyReviewRoundAdoption(ctx, in.IdempotencyKey, in.PreviewSHA256, in.Document)
	case "review_obligations_list":
		return c.AdoptionObligations(ctx, in.PullNumber, in.Status, page)
	case "review_obligation_read":
		return c.AdoptionObligation(ctx, in.ObligationID)
	case "review_obligations_import_preview":
		return c.PreviewAdoptionObligations(ctx, in.Document)
	case "review_obligations_import_apply":
		return c.ApplyAdoptionObligations(ctx, in.IdempotencyKey, in.PreviewSHA256, in.Document)
	case "review_obligation_resolve":
		return c.ResolveAdoptionObligation(ctx, in.ObligationID, in.IdempotencyKey, a.resolution)
	case "history_collection_enqueue":
		return c.CollectHistory(ctx, in.IdempotencyKey, client.HistorySelection{PullNumbers: in.PullNumbers}, in.WaitSeconds)
	case "history_collection_read":
		return c.HistoryCollection(ctx, in.CollectionID, in.WaitSeconds)
	case "history_collection_resume", "history_collection_cancel":
		return c.ChangeHistoryCollection(ctx, in.CollectionID, strings.TrimPrefix(a.name, "history_collection_"), in.IdempotencyKey, *in.ExpectedRevision)
	case "history_collection_pages":
		return c.HistoryPages(ctx, in.CollectionID, in.Limit, in.Cursor)
	case "history_collection_capture_read":
		return c.HistoryCapture(ctx, in.CollectionID, in.PageID)
	case "map_import_preview":
		return c.PreviewMapImport(ctx, in.Document)
	case "map_import_apply":
		return c.ApplyMapImport(ctx, in.IdempotencyKey, in.PreviewSHA256, in.Document)
	case "map_import_read":
		return c.MapImport(ctx, in.SourceKey)
	case "map_import_history":
		return c.MapImportHistory(ctx, in.SourceKey, page)
	case "map_import_revision_read":
		return c.MapImportRevision(ctx, in.SourceKey, in.Revision)
	case "review_queue_edit":
		return c.EditReviewQueue(ctx, in.IdempotencyKey, in.Document)
	}
	return nil, invalidArguments()
}
