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

func TestPresentationReplacementRequiresExplicitRevision(t *testing.T) {
	for name, document := range map[string]string{
		"missing":          `{"presentation":{"name":"Selected"}}`,
		"null":             `{"expected_revision":null,"presentation":{"name":"Selected"}}`,
		"duplicate":        `{"expected_revision":0,"expected_revision":1,"presentation":{"name":"Selected"}}`,
		"unknown":          `{"expected_revision":0,"unknown":true,"presentation":{"name":"Selected"}}`,
		"mis-cased":        `{"Expected_Revision":0,"presentation":{"name":"Selected"}}`,
		"string":           `{"expected_revision":"0","presentation":{"name":"Selected"}}`,
		"negative":         `{"expected_revision":-1,"presentation":{"name":"Selected"}}`,
		"fractional":       `{"expected_revision":0.5,"presentation":{"name":"Selected"}}`,
		"exhausted":        `{"expected_revision":9223372036854775807,"presentation":{"name":"Selected"}}`,
		"nested-unknown":   `{"expected_revision":0,"presentation":{"name":"Selected","unknown":true}}`,
		"nested-duplicate": `{"expected_revision":0,"presentation":{"name":"Selected","name":"Other"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := `{"document":` + document + `,"idempotency_key":"chosen"}`
			if _, err := parseExistingManagementArguments("presentation_replace", []byte(raw)); err == nil {
				t.Fatal("malformed revision document accepted", document)
			}
		})
	}
	for _, revision := range []string{"0", "1", "9223372036854775806"} {
		raw := `{"document":{"expected_revision":` + revision + `,"presentation":{"name":"Selected"}},"idempotency_key":"chosen"}`
		args, err := parseExistingManagementArguments("presentation_replace", []byte(raw))
		if err != nil || args == nil {
			t.Fatal("explicit valid revision rejected", revision, err)
		}
		encoded, err := json.Marshal(args.presentation.ExpectedRevision)
		if err != nil || string(encoded) != revision {
			t.Fatal("explicit revision changed", revision, string(encoded), err)
		}
	}
}

func TestPresentationReplacementRejectsExplicitNullConfig(t *testing.T) {
	for _, field := range []string{"name", "brand_name", "tagline", "headings", "theme"} {
		t.Run(field, func(t *testing.T) {
			config := map[string]any{"name": "Selected", field: nil}
			raw, err := json.Marshal(map[string]any{"document": map[string]any{"expected_revision": 7, "presentation": config}, "idempotency_key": "chosen"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseExistingManagementArguments("presentation_replace", raw); err == nil {
				t.Fatal("explicit null accepted", field)
			}
		})
	}
	raw := `{"document":{"expected_revision":7,"presentation":{"name":"Selected","brand_name":"Custom","tagline":"Custom tagline","headings":{},"theme":{}}},"idempotency_key":"chosen"}`
	args, err := parseExistingManagementArguments("presentation_replace", []byte(raw))
	if err != nil || args.presentation.ExpectedRevision != 7 || args.presentation.Presentation.BrandName != "Custom" {
		t.Fatal("valid replacement changed", args, err)
	}
}
