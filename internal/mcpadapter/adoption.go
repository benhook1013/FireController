package mcpadapter

// Adoption tools expose only bound carried-round and obligation operations.
// Documents are native operation inputs, never URLs, HTTP verbs or credentials.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type adoptionInput struct {
	Document         json.RawMessage `json:"document,omitempty"`
	IdempotencyKey   string          `json:"idempotency_key,omitempty"`
	PreviewSHA256    string          `json:"preview_sha256,omitempty"`
	ExpectedRevision *int64          `json:"expected_revision,omitempty"`
	Limit            int             `json:"limit,omitempty"`
	Cursor           string          `json:"cursor,omitempty"`
	PullNumber       int64           `json:"pull_number,omitempty"`
	Status           string          `json:"status,omitempty"`
	ObligationID     string          `json:"obligation_id,omitempty"`
	TargetPullNumber *int64          `json:"target_pull_number,omitempty"`
	Reason           string          `json:"reason,omitempty"`
}
type adoptionArgs struct {
	name       string
	input      adoptionInput
	resolution client.AdoptionObligationResolveRequest
	retarget   client.AdoptionObligationRetargetRequest
}
type adoptionSpec struct {
	name, description            string
	fields, required             []string
	write, destructive, external bool
}

var adoptionSpecs = []adoptionSpec{
	{"review_round_adoption_preview", "Preview the existing native round-adoption document against its original-source and current revision guards; no live reviews are requested.", []string{"document"}, []string{"document"}, false, false, false},
	{"review_round_adoption_apply", "Apply the exact previewed native round-adoption document with preview_sha256 and explicit key. Preserves retained attribution and accounting rules; tooling access is not review authorization.", []string{"document", "preview_sha256", "idempotency_key"}, []string{"document", "preview_sha256", "idempotency_key"}, true, true, false},
	{"review_obligations_list", "Read one compact carried-obligation page, optionally scoped to pull_number and open/resolved status. Full bodies require review_obligation_read.", []string{"pull_number", "status", "limit", "cursor"}, nil, false, false, false},
	{"review_obligation_read", "Read one explicit retained carried obligation and original evidence, without marking it resolved.", []string{"obligation_id"}, []string{"obligation_id"}, false, false, false},
	{"review_obligations_import_preview", "Preview one strict native carried-obligation batch, preserving original evidence and provenance; no application or new review.", []string{"document"}, []string{"document"}, false, false, false},
	{"review_obligations_import_apply", "Apply the exact previewed carried-obligation batch with preview_sha256 and explicit key. Existing source conflicts and evidence requirements remain enforced.", []string{"document", "preview_sha256", "idempotency_key"}, []string{"document", "preview_sha256", "idempotency_key"}, true, true, false},
	{"review_obligation_retarget", "Change the receiving PR of one open carried route with expected_revision, explicit target_pull_number (zero means unassigned backlog), reason and key. Preserves original provenance and open status; no provider work or resolution. Findings/results cannot be retargeted.", []string{"obligation_id", "expected_revision", "target_pull_number", "reason", "idempotency_key"}, []string{"obligation_id", "expected_revision", "target_pull_number", "reason", "idempotency_key"}, true, true, false},
	{"review_obligation_resolve", "Resolve one carried obligation using the native {expected_revision,reason,evidence} document and explicit key. Requires a real evidence-backed resolution; grants no review credit.", []string{"obligation_id", "document", "idempotency_key"}, []string{"obligation_id", "document", "idempotency_key"}, true, true, false},
}

func adoptionSpecFor(name string) (adoptionSpec, bool) {
	for _, s := range adoptionSpecs {
		if s.name == name {
			return s, true
		}
	}
	return adoptionSpec{}, false
}
func adoptionTool(name string) bool { _, ok := adoptionSpecFor(name); return ok }
func adoptionWrite(name string) bool {
	s, ok := adoptionSpecFor(name)
	return ok && s.write
}
func adoptionDestructive(name string) bool {
	s, ok := adoptionSpecFor(name)
	return ok && s.destructive
}
func adoptionDefinitions() []definition {
	var out []definition
	for _, s := range adoptionSpecs {
		properties := map[string]any{}
		for _, name := range s.fields {
			p := map[string]any{"type": "string", "minLength": 1, "maxLength": 128}
			switch name {
			case "document":
				p = map[string]any{"type": "object", "description": "Strict native operation document. The existing typed client validates its exact fields, values and byte budget before any service call."}
			case "expected_revision":
				p = map[string]any{"type": "integer", "minimum": 0, "maximum": int64(math.MaxInt64 - 1)}
			case "pull_number":
				p = map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}
			case "target_pull_number":
				p = map[string]any{"type": "integer", "minimum": 0, "maximum": 2147483647}
			case "reason":
				p = map[string]any{"type": "string", "minLength": 1, "maxLength": 4096}
			case "limit":
				p = map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
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
func parseAdoptionArguments(name string, raw json.RawMessage) (*adoptionArgs, error) {
	spec, ok := adoptionSpecFor(name)
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
	a := &adoptionArgs{name: name}
	if strictjson.Decode(raw, &a.input) != nil {
		return nil, invalidArguments()
	}
	in := &a.input
	if _, ok := fields["idempotency_key"]; ok && !validKey(in.IdempotencyKey) {
		return nil, invalidArguments()
	}
	if _, ok := fields["preview_sha256"]; ok && !adoptionHash(in.PreviewSHA256) {
		return nil, invalidArguments()
	}
	if in.ExpectedRevision != nil && (*in.ExpectedRevision < 0 || *in.ExpectedRevision == math.MaxInt64) {
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
	if in.ObligationID != "" && client.ValidateAdoptionObligationID(in.ObligationID) != nil {
		return nil, invalidArguments()
	}
	var err error
	switch name {
	case "review_round_adoption_preview":
		err = client.ValidateReviewRoundAdoptionDocument(in.Document)
	case "review_round_adoption_apply":
		err = client.ValidateReviewRoundAdoptionApply(in.IdempotencyKey, in.PreviewSHA256, in.Document)
	case "review_obligations_import_preview":
		in.Document, err = client.NormalizeAdoptionObligationDocument(in.Document)
	case "review_obligations_import_apply":
		err = client.ValidateAdoptionObligationApply(in.IdempotencyKey, in.PreviewSHA256, in.Document)
	case "review_obligation_retarget":
		a.retarget, err = client.NormalizeAdoptionObligationRetarget(client.AdoptionObligationRetargetRequest{ExpectedRevision: in.ExpectedRevision, TargetPullNumber: in.TargetPullNumber, Reason: in.Reason})
	case "review_obligation_resolve":
		err = strictjson.Decode(in.Document, &a.resolution)
		if err == nil {
			a.resolution, err = client.NormalizeAdoptionObligationResolution(a.resolution)
		}

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
func adoptionHash(value string) bool {
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
func executeAdoption(ctx context.Context, c *client.Client, a *adoptionArgs) (any, error) {
	in := a.input
	page := client.PageOptions{Limit: in.Limit, Cursor: in.Cursor}
	switch a.name {
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
	case "review_obligation_retarget":
		return c.RetargetAdoptionObligation(ctx, in.ObligationID, in.IdempotencyKey, a.retarget)
	case "review_obligation_resolve":
		return c.ResolveAdoptionObligation(ctx, in.ObligationID, in.IdempotencyKey, a.resolution)
	}
	return nil, invalidArguments()
}
