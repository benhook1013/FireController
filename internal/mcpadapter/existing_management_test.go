package mcpadapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExistingManagementSchemasAndMalformedInputsStayLocal(t *testing.T) {
	names := map[string]bool{}
	for _, d := range existingManagementDefinitions() {
		if names[d.name] || d.schema["additionalProperties"] != false {
			t.Fatal("ambiguous management tool", d.name)
		}
		names[d.name] = true
		for _, raw := range []json.RawMessage{[]byte(`null`), []byte(`{"project_id":"other-project"}`), []byte(`{"document":{},"idempotency_key":"a","idempotency_key":"b"}`)} {
			if _, err := parseExistingManagementArguments(d.name, raw); err == nil {
				t.Fatal("unsafe outer arguments accepted", d.name, string(raw))
			}
		}
	}
	for name, raw := range map[string]string{
		"publication_unpublish":      `{"expected_revision":null,"idempotency_key":"chosen"}`,
		"history_collection_read":    `{"collection_id":""}`,
		"history_collection_enqueue": `{"pull_numbers":[2,2],"idempotency_key":"chosen"}`,
		"presentation_replace":       `{"document":{},"idempotency_key":"chosen"}`,
		"review_import_apply":        `{"document":{},"preview_sha256":"short","idempotency_key":"chosen"}`,
		"review_obligations_list":    `{"status":""}`,
		"map_import_read":            `{"source_key":"../escape"}`,
		"history_collection_pages":   `{"collection_id":"history_collection_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","limit":0}`,
	} {
		if _, err := parseExistingManagementArguments(name, []byte(raw)); err == nil {
			t.Fatal("malformed native input accepted", name)
		}
	}
}

func TestReviewManagementToolDescriptionsStateAuthority(t *testing.T) {
	want := map[string]bool{
		"review_import_preview":             false,
		"review_import_apply":               false,
		"review_transfer_preview":           false,
		"review_transfer_apply":             false,
		"review_round_adoption_preview":     true,
		"review_round_adoption_apply":       true,
		"review_obligations_import_preview": true,
		"review_obligations_import_apply":   true,
		"review_queue_edit":                 false,
	}
	definitions := existingManagementDefinitions()
	for _, d := range definitions {
		workerRequired, ok := want[d.name]
		if !ok {
			continue
		}
		if !strings.Contains(d.description, "Requires project management or instance administration") || workerRequired && !strings.Contains(d.description, "valid project worker") {
			t.Errorf("%s omits its required authority: %s", d.name, d.description)
		}
		delete(want, d.name)
	}
	if len(want) != 0 {
		t.Fatalf("management tools missing from discovery: %v", want)
	}
}
