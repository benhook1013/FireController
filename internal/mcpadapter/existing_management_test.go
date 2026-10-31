package mcpadapter

import (
	"encoding/json"
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
