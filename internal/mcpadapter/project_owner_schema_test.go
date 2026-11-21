package mcpadapter

import (
	"encoding/json"
	"github.com/google/jsonschema-go/jsonschema"
	"reflect"
	"testing"
)

func TestOwnerManagementSchemasNeverEmitNullRequired(t *testing.T) {
	expected := map[string][]string{
		"review_policy_read":         nil,
		"review_policy_configure":    {"idempotency_key", "expected_revision", "required_channels", "max_review_files", "paused_channels"},
		"review_execution_configure": {"idempotency_key", "expected_revision", "capability_id", "mode", "queue", "channels", "paused"},
		"worker_retirement_read":     {"worker_id"},
		"worker_retire":              {"worker_id", "idempotency_key"},
		"worker_create":              {"idempotency_key", "name"},
		"project_credential_grant":   {"idempotency_key", "token_sha256", "permissions"},
		"project_credential_revoke":  {"idempotency_key", "credential_id"},
		"project_credential_update":  {"idempotency_key", "credential_id", "expected_permissions", "permissions"},
	}
	for _, def := range ownerManagementDefinitions() {
		t.Run(def.name, func(t *testing.T) {
			want, known := expected[def.name]
			if !known {
				t.Fatalf("missing required-field expectation for %s", def.name)
			}
			if got, _ := def.schema["required"].([]string); !reflect.DeepEqual(got, want) {
				t.Fatalf("required fields: want %v, got %v", want, got)
			}
			raw, err := json.Marshal(def.schema)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			required, present := decoded["required"]
			if def.name == "review_policy_read" {
				if present {
					t.Fatalf("argument-free tool unexpectedly has required: %v", required)
				}
				return
			}
			values, ok := required.([]any)
			if !present || !ok || len(values) == 0 {
				t.Fatalf("required arguments lost: %s", raw)
			}
			props := decoded["properties"].(map[string]any)
			for _, value := range values {
				name, ok := value.(string)
				if !ok {
					t.Fatalf("non-string required argument: %v", value)
				}
				if _, ok := props[name]; !ok {
					t.Fatalf("missing required property %q", name)
				}
			}
		})
	}
}

func TestOwnerPermissionSchemasRequireReadOrManage(t *testing.T) {
	for _, def := range ownerManagementDefinitions() {
		props := def.schema["properties"].(map[string]any)
		for _, field := range []string{"permissions", "expected_permissions"} {
			raw, exists := props[field]
			if !exists {
				continue
			}
			data, _ := json.Marshal(raw)
			var schema jsonschema.Schema
			if err := json.Unmarshal(data, &schema); err != nil {
				t.Fatal(err)
			}
			resolved, err := schema.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				values []any
				valid  bool
			}{
				{[]any{"write"}, false}, {[]any{"read", "write"}, true}, {[]any{"manage"}, true},
			} {
				err := resolved.Validate(tc.values)
				if (err == nil) != tc.valid {
					t.Errorf("%s.%s permissions %v: valid=%v, error=%v", def.name, field, tc.values, tc.valid, err)
				}
			}
		}
	}
}
